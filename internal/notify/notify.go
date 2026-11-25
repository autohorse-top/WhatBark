// Package notify delivers V-4 notifications: task-terminal and mention
// events go out asynchronously over SMTP and/or a per-group webhook URL.
//
// Design rules (TASK.md V-4):
//   - SMTP settings come from the settings table (smtp_host/smtp_port/
//     smtp_from/smtp_user/smtp_pass). Credentials may be overridden by the
//     WHATBARK_SMTP_USER / WHATBARK_SMTP_PASS env vars (secret-first-env);
//     the env value always wins when set.
//   - SMTP unset (empty smtp_host) => mail is silently skipped (log only),
//     never an error.
//   - The webhook fires only for group events (task.group_id or
//     message.group_id set) when that group's webhook_url is non-empty;
//     non-2xx responses and network errors are logged, never propagated.
//   - All work happens in a goroutine; the caller (Flow) is never blocked
//     or failed by notification problems.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"whatbark/internal/store"
)

const (
	defaultSMTPPort = 587
	webhookTimeout  = 10 * time.Second
	webhookMaxRead  = 1 << 20
	excerptRunes    = 200
)

// Notifier resolves recipients and ships notifications. Store access
// happens on the worker goroutine, so callers can pass fresh task/message
// ids immediately after their own write (SQLite is single-writer; the
// goroutine queues behind it).
type Notifier struct {
	Store  *store.Store
	Client *http.Client
	// Logf receives an already-prefixed message (e.g. "task 7 mail to x").
	Logf func(format string, args ...any)
}

// New wires a Notifier with sane defaults (10s webhook timeout, slog.Info).
func New(st *store.Store) *Notifier {
	return &Notifier{
		Store:  st,
		Client: &http.Client{Timeout: webhookTimeout},
		// Callers use fmt-style verbs ("... %s", x); Sprintf keeps the
		// message intact and keeps slog out of format-string territory.
		Logf: func(format string, args ...any) { slog.Info(fmt.Sprintf(format, args...)) },
	}
}

// OnTaskTerminal fires an async notification for a task that just reached
// a terminal state (done/failed). Never blocks, never fails the caller.
func (n *Notifier) OnTaskTerminal(taskID int, status string) {
	go n.taskTerminal(taskID, status)
}

// OnMention fires an async notification for a message that @-mentioned
// employees. The store has already recorded the mention rows (the sender
// is excluded by the store); messages with no real mention are no-ops.
func (n *Notifier) OnMention(msgID int) {
	go n.mention(msgID)
}

// taskTerminal is the synchronous worker for OnTaskTerminal (exported for
// tests so they can run it inline and assert deterministically).
func (n *Notifier) taskTerminal(taskID int, status string) {
	t, err := n.Store.GetTask(taskID)
	if err != nil {
		n.Logf("notify: task %d lookup: %v", taskID, err)
		return
	}
	// Recipients: the task owner plus the requester (deduped, >0 only —
	// 0 is the boss who has no employees row and therefore no mailbox).
	ids := []int{}
	if t.EmpID > 0 {
		ids = append(ids, t.EmpID)
	}
	if t.RequestedBy > 0 && t.RequestedBy != t.EmpID {
		ids = append(ids, t.RequestedBy)
	}
	subject := fmt.Sprintf("[whatbark] task %d %s (%s)", taskID, status, t.Kind)
	body := fmt.Sprintf("Task %d (%s) for employee %d reached status %q.\n\ninput: %s\n\noutput: %s\n",
		taskID, t.Kind, t.EmpID, status, excerpt(t.Input), excerpt(t.Output))
	n.emailRecipients(ids, subject, body)
	if t.GroupID != nil {
		payload := map[string]any{
			"type":        "task.status",
			"task_id":     taskID,
			"status":      status,
			"kind":        t.Kind,
			"employee_id": t.EmpID,
			"group_id":    *t.GroupID,
		}
		if out := excerpt(t.Output); out != "" {
			payload["output_excerpt"] = out
		}
		n.postGroupWebhook(*t.GroupID, payload)
	}
}

// mention is the synchronous worker for OnMention (exported for tests).
func (n *Notifier) mention(msgID int) {
	mids, err := n.Store.MessageMentions(msgID)
	if err != nil {
		n.Logf("notify: message %d mentions lookup: %v", msgID, err)
		return
	}
	if len(mids) == 0 {
		return
	}
	msg, err := n.Store.GetMessage(msgID)
	if err != nil {
		n.Logf("notify: message %d lookup: %v", msgID, err)
		return
	}
	var sender int
	if msg.FromID != nil {
		sender = *msg.FromID
	}
	subject := fmt.Sprintf("[whatbark] %s mentioned you (msg %d)", nameOrID(sender), msgID)
	body := fmt.Sprintf("Employee %s mentioned you in message %d:\n\n%s\n",
		nameOrID(sender), msgID, excerpt(msg.Body))
	n.emailRecipients(mids, subject, body)
	if msg.GroupID != nil {
		payload := map[string]any{
			"type":       "mention",
			"message_id": msgID,
			"group_id":   *msg.GroupID,
			"mentioned":  mids,
		}
		if msg.FromID != nil {
			payload["sender"] = *msg.FromID
		} else {
			payload["sender"] = nil
		}
		n.postGroupWebhook(*msg.GroupID, payload)
	}
}

// emailRecipients mails every recipient with a stored mailbox; silently
// skips (log only) when SMTP is not configured. Per-recipient failures are
// logged and do not stop the loop.
func (n *Notifier) emailRecipients(ids []int, subject, body string) {
	if _, _, _, _, _, ok := n.smtpConfig(); !ok {
		n.Logf("notify: smtp not configured, skip mail (%s)", subject)
		return
	}
	mails, err := n.Store.EmployeeEmails(ids)
	if err != nil {
		n.Logf("notify: resolve emails: %v", err)
		return
	}
	idlist := make([]int, 0, len(mails))
	for id := range mails {
		idlist = append(idlist, id)
	}
	sort.Ints(idlist)
	for _, id := range idlist {
		if err := n.sendMail(mails[id], subject, body); err != nil {
			n.Logf("notify: mail to %s (emp %d): %v", mails[id], id, err)
		} else {
			n.Logf("notify: mail sent to %s (emp %d)", mails[id], id)
		}
	}
}

// sendMail ships one SMTP mail. host/port/from/user/pass are already
// resolved (see smtpConfig).
func (n *Notifier) sendMail(to, subject, body string) error {
	host, port, from, user, pass, ok := n.smtpConfig()
	if !ok {
		return fmt.Errorf("smtp not configured")
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if user != "" {
		auth := smtp.PlainAuth("", user, pass, host)
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := "From: " + from +
		"\r\nTo: " + to +
		"\r\nSubject: " + subject +
		"\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n" + body
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	// w.Close sends the RFC 5321 terminator (\r\n.\r\n) and blocks until the
	// server acknowledges with 250, so sendMail only returns once the server
	// has accepted the message.
	return w.Close()
}

// smtpConfig resolves SMTP settings: DB values with env override for the
// credentials (WHATBARK_SMTP_USER / WHATBARK_SMTP_PASS — secret-first-env).
// ok=false when smtp_host is empty (SMTP not configured).
func (n *Notifier) smtpConfig() (host string, port int, from, user, pass string, ok bool) {
	host = n.Store.GetSettingDefault("smtp_host", "")
	if host == "" {
		return "", 0, "", "", "", false
	}
	port = defaultSMTPPort
	if p, err := strconv.Atoi(n.Store.GetSettingDefault("smtp_port", "")); err == nil && p > 0 && p <= 65535 {
		port = p
	}
	from = n.Store.GetSettingDefault("smtp_from", "whatbark@localhost")
	user = n.Store.GetSettingDefault("smtp_user", "")
	pass = n.Store.GetSettingDefault("smtp_pass", "")
	if env := os.Getenv("WHATBARK_SMTP_USER"); env != "" {
		user = env
	}
	if env := os.Getenv("WHATBARK_SMTP_PASS"); env != "" {
		pass = env
	}
	return host, port, from, user, pass, true
}

// postGroupWebhook POSTs the event payload to the group's webhook_url
// (no-op when the group has none). Non-2xx and network errors are logged.
func (n *Notifier) postGroupWebhook(groupID int, payload map[string]any) {
	g, err := n.Store.GetGroup(groupID)
	if err != nil {
		n.Logf("notify: group %d webhook lookup: %v", groupID, err)
		return
	}
	if g.WebhookURL == "" {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		n.Logf("notify: group %d webhook payload: %v", groupID, err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, g.WebhookURL, bytes.NewReader(b))
	if err != nil {
		n.Logf("notify: group %d webhook request: %v", groupID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.Client.Do(req)
	if err != nil {
		n.Logf("notify: group %d webhook post: %v", groupID, err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookMaxRead))
	n.Logf("notify: group %d webhook -> %s", groupID, resp.Status)
}

// excerpt trims s to at most n runes (UTF-8 safe) for event payloads.
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= excerptRunes {
		return s
	}
	return string(r[:excerptRunes]) + "…"
}

// nameOrID renders a sender for mail subjects: the name when known,
// "boss" for id 0, the id otherwise.
func nameOrID(sender int) string {
	if sender == 0 {
		return "boss"
	}
	return strconv.Itoa(sender)
}
