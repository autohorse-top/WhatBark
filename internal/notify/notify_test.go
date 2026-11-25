package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"whatbark/internal/store"
)

// ---------- test sinks ----------

type smtpMail struct {
	to  string
	raw string // full DATA payload (headers + body)
}

// smtpSink is a minimal in-process ESMTP server: it accepts one or more
// connections, answers EHLO/MAIL/RCPT/DATA/QUIT, and records each message.
type smtpSink struct {
	mu    sync.Mutex
	mails []smtpMail
	lis   net.Listener
}

func newSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("smtp sink listen: %v", err)
	}
	s := &smtpSink{lis: lis}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go s.serveConn(conn)
		}
	}()
	t.Cleanup(func() { _ = lis.Close() })
	return s
}

func (s *smtpSink) hostPort() (string, int) {
	a := s.lis.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func (s *smtpSink) get() []smtpMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]smtpMail, len(s.mails))
	copy(out, s.mails)
	return out
}

func (s *smtpSink) serveConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	fmt.Fprint(conn, "220 sink ESMTP\r\n")
	var m smtpMail
	var data []string
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case inData:
			if line == "." {
				inData = false
				m.raw = strings.Join(data, "\n")
				data = nil
				s.mu.Lock()
				s.mails = append(s.mails, m)
				s.mu.Unlock()
				fmt.Fprint(conn, "250 OK\r\n")
			} else {
				data = append(data, line)
			}
		case strings.HasPrefix(line, "MAIL FROM:"):
			fmt.Fprint(conn, "250 OK\r\n")
		case strings.HasPrefix(line, "RCPT TO:"):
			m.to = strings.TrimPrefix(line, "RCPT TO:")
			m.to = strings.TrimSpace(strings.Trim(m.to, "<>"))
			fmt.Fprint(conn, "250 OK\r\n")
		case strings.EqualFold(line, "DATA"):
			inData = true
			fmt.Fprint(conn, "354 go ahead\r\n")
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"),
			strings.EqualFold(line, "RSET"), strings.EqualFold(line, "NOOP"):
			fmt.Fprint(conn, "250 sink\r\n")
		case strings.EqualFold(line, "QUIT"):
			fmt.Fprint(conn, "221 bye\r\n")
			return
		default:
			fmt.Fprint(conn, "250 OK\r\n")
		}
	}
}

type hookSink struct {
	mu       sync.Mutex
	payloads []map[string]any
	srv      *httptest.Server
}

func newHookSink(t *testing.T) *hookSink {
	t.Helper()
	h := &hookSink{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.payloads = append(h.payloads, p)
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookSink) payload() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]map[string]any, len(h.payloads))
	copy(out, h.payloads)
	return out
}

type logSink struct {
	mu      sync.Mutex
	entries []string
}

func (l *logSink) appendf(format string, args ...any) {
	l.mu.Lock()
	l.entries = append(l.entries, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logSink) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.entries))
	copy(out, l.entries)
	return out
}

func (l *logSink) contains(substr string) bool {
	for _, ln := range l.lines() {
		if strings.Contains(ln, substr) {
			return true
		}
	}
	return false
}

// ---------- fixture ----------

type fixture struct {
	st     *store.Store
	leader int
	worker int
	gid    int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root, err := st.CreateDepartment("root", nil)
	if err != nil {
		t.Fatalf("CreateDepartment: %v", err)
	}
	leader, err := st.CreateEmployee("alice", "leader", store.KindAI, root)
	if err != nil {
		t.Fatalf("CreateEmployee leader: %v", err)
	}
	worker, err := st.CreateEmployee("bob", "worker", store.KindAI, root)
	if err != nil {
		t.Fatalf("CreateEmployee worker: %v", err)
	}
	if err := st.SetDepartmentLeader(root, leader); err != nil {
		t.Fatalf("SetDepartmentLeader: %v", err)
	}
	if err := st.SetEmail(leader, "alice@example.com"); err != nil {
		t.Fatalf("SetEmail leader: %v", err)
	}
	if err := st.SetEmail(worker, "bob@example.com"); err != nil {
		t.Fatalf("SetEmail worker: %v", err)
	}
	gid, err := st.CreateGroup("v4-g", leader, []int{worker}, "group")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return &fixture{st: st, leader: leader, worker: worker, gid: gid}
}

func pint(v int) *int { return &v }

// ---------- tests ----------

// TestTaskTerminalMailAndWebhook (V-4): a group task reaching a terminal
// state mails both the owner and the requester and posts the task.status
// event to the group webhook.
func TestTaskTerminalMailAndWebhook(t *testing.T) {
	f := newFixture(t)
	hook := newHookSink(t)
	if err := f.st.SetGroupWebhook(context.Background(), f.gid, &hook.srv.URL); err != nil {
		t.Fatalf("SetGroupWebhook: %v", err)
	}
	sm := newSMTPSink(t)
	host, port := sm.hostPort()
	if err := f.st.SetSetting("smtp_host", host); err != nil {
		t.Fatalf("SetSetting smtp_host: %v", err)
	}
	if err := f.st.SetSetting("smtp_port", strconv.Itoa(port)); err != nil {
		t.Fatalf("SetSetting smtp_port: %v", err)
	}
	if err := f.st.SetSetting("smtp_from", "whatbark@example.com"); err != nil {
		t.Fatalf("SetSetting smtp_from: %v", err)
	}

	logs := &logSink{}
	n := New(f.st)
	n.Logf = logs.appendf

	taskID, err := f.st.CreateTask(f.worker, store.KindDispatch, "do the thing", nil, nil, f.leader, 5, pint(f.gid))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	n.taskTerminal(taskID, store.TaskDone)

	mails := sm.get()
	if len(mails) != 2 {
		t.Fatalf("got %d mails, want 2 (owner + requester): %+v (logs: %v)", len(mails), mails, logs.lines())
	}
	seen := map[string]bool{}
	subj := fmt.Sprintf("Subject: [whatbark] task %d %s (dispatch)", taskID, store.TaskDone)
	for _, m := range mails {
		if m.to != "alice@example.com" && m.to != "bob@example.com" {
			t.Errorf("unexpected recipient %q", m.to)
		}
		seen[m.to] = true
		if !strings.Contains(m.raw, subj) {
			t.Errorf("mail to %s missing subject %q:\n%s", m.to, subj, m.raw)
		}
		if !strings.Contains(m.raw, "do the thing") {
			t.Errorf("mail to %s missing input excerpt", m.to)
		}
	}
	if !seen["alice@example.com"] || !seen["bob@example.com"] {
		t.Errorf("recipients = %v, want both alice and bob", seen)
	}

	pl := hook.payload()
	if len(pl) != 1 {
		t.Fatalf("got %d webhook payloads, want 1: %+v", len(pl), pl)
	}
	p := pl[0]
	if p["type"] != "task.status" || p["status"] != store.TaskDone ||
		p["task_id"] != float64(taskID) || p["employee_id"] != float64(f.worker) ||
		p["group_id"] != float64(f.gid) {
		t.Errorf("payload = %v, want task.status/done/%d/%d/%d", p, taskID, f.worker, f.gid)
	}
}

// TestTaskTerminalNoSMTP (V-4): with no smtp_host configured the mail step
// is skipped (logged) and the group webhook still fires — no error anywhere.
func TestTaskTerminalNoSMTP(t *testing.T) {
	f := newFixture(t)
	hook := newHookSink(t)
	if err := f.st.SetGroupWebhook(context.Background(), f.gid, &hook.srv.URL); err != nil {
		t.Fatalf("SetGroupWebhook: %v", err)
	}
	logs := &logSink{}
	n := New(f.st)
	n.Logf = logs.appendf

	taskID, err := f.st.CreateTask(f.worker, store.KindDispatch, "x", nil, nil, 0, 0, pint(f.gid))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	n.taskTerminal(taskID, store.TaskFailed)

	if !logs.contains("smtp not configured") {
		t.Errorf("logs = %v, want an 'smtp not configured' skip note", logs.lines())
	}
	pl := hook.payload()
	if len(pl) != 1 || pl[0]["type"] != "task.status" || pl[0]["status"] != store.TaskFailed {
		t.Errorf("payload = %+v, want one task.status/failed event", pl)
	}
}

// TestTaskTerminalSMTPFailure (V-4): a dead SMTP endpoint must not panic or
// propagate — only a failure log line is produced.
func TestTaskTerminalSMTPFailure(t *testing.T) {
	f := newFixture(t)
	// Reserve then close a local port so dialing it is refused.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()
	if err := f.st.SetSetting("smtp_host", "127.0.0.1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := f.st.SetSetting("smtp_port", strconv.Itoa(port)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	logs := &logSink{}
	n := New(f.st)
	n.Logf = logs.appendf

	taskID, err := f.st.CreateTask(f.worker, store.KindDispatch, "x", nil, nil, 0, 0, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	n.taskTerminal(taskID, store.TaskDone) // must not panic

	if !logs.contains("mail to") {
		t.Errorf("logs = %v, want a 'mail to ...' failure line", logs.lines())
	}
}

// TestMentionMailAndWebhook (V-4): a 1:1 mention mails the mentioned
// employee (no webhook — no group); a group mention posts the mention event
// to the group webhook and mails the mentioned member.
func TestMentionMailAndWebhook(t *testing.T) {
	f := newFixture(t)
	hook := newHookSink(t)
	if err := f.st.SetGroupWebhook(context.Background(), f.gid, &hook.srv.URL); err != nil {
		t.Fatalf("SetGroupWebhook: %v", err)
	}
	sm := newSMTPSink(t)
	host, port := sm.hostPort()
	if err := f.st.SetSetting("smtp_host", host); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := f.st.SetSetting("smtp_port", strconv.Itoa(port)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	logs := &logSink{}
	n := New(f.st)
	n.Logf = logs.appendf

	// 1:1 mention: worker -> leader, body @alice (case-sensitive name match).
	msgID, err := f.st.CreateMessage(f.leader, store.MsgChat, "@alice ping", pint(f.worker), nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	mids, err := f.st.MessageMentions(msgID)
	if err != nil || len(mids) != 1 || mids[0] != f.leader {
		t.Fatalf("MessageMentions = %v (err %v), want [%d]", mids, err, f.leader)
	}
	n.mention(msgID)

	mails := sm.get()
	if len(mails) != 1 {
		t.Fatalf("got %d mails, want 1: %+v", len(mails), mails)
	}
	if mails[0].to != "alice@example.com" {
		t.Errorf("mail to = %q, want alice@example.com", mails[0].to)
	}
	if len(hook.payload()) != 0 {
		t.Errorf("1:1 mention must not fire the group webhook: %+v", hook.payload())
	}

	// Group mention: worker posts @alice in the group -> webhook + mail.
	gmsg, err := f.st.CreateGroupMessage(f.gid, store.MsgChat, "@alice hi there", pint(f.worker), nil, nil)
	if err != nil {
		t.Fatalf("CreateGroupMessage: %v", err)
	}
	gmids, err := f.st.MessageMentions(gmsg)
	if err != nil || len(gmids) != 1 || gmids[0] != f.leader {
		t.Fatalf("group MessageMentions = %v (err %v), want [%d]", gmids, err, f.leader)
	}
	n.mention(gmsg)

	mails = sm.get()
	if len(mails) != 2 {
		t.Fatalf("got %d mails after group mention, want 2: %+v", len(mails), mails)
	}
	pl := hook.payload()
	if len(pl) != 1 {
		t.Fatalf("got %d webhook payloads after group mention, want 1: %+v", len(pl), pl)
	}
	p := pl[0]
	if p["type"] != "mention" || p["message_id"] != float64(gmsg) ||
		p["group_id"] != float64(f.gid) || p["sender"] != float64(f.worker) {
		t.Errorf("mention payload = %v, want type=mention msg=%d group=%d sender=%d", p, gmsg, f.gid, f.worker)
	}
	mentioned, _ := p["mentioned"].([]any)
	if len(mentioned) != 1 || mentioned[0] != float64(f.leader) {
		t.Errorf("mentioned = %v, want [%d]", mentioned, f.leader)
	}
	if !logs.contains("mail sent to alice@example.com") {
		t.Errorf("logs = %v, want both mention mails delivered", logs.lines())
	}
}
