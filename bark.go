package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// barkClient talks to the platform API with a bearer token.
type barkClient struct {
	url   string
	token string
	http  *http.Client
}

func newBarkClient(serverURL, token string) *barkClient {
	return &barkClient{url: serverURL, token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// barkErr is a human-readable API failure.
type barkErr struct {
	status int
	msg    string
}

func (e *barkErr) Error() string { return fmt.Sprintf("http %d: %s", e.status, e.msg) }

// do performs a JSON request and decodes the response into out (optional).
func (c *barkClient) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(buf, &e)
		msg := e.Error
		if msg == "" {
			msg = resp.Status
		}
		return &barkErr{status: resp.StatusCode, msg: msg}
	}
	if out != nil && len(buf) > 0 {
		return json.Unmarshal(buf, out)
	}
	return nil
}

// printJSON writes v as compact JSON to stdout (machine-readable for agents).
func printJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("marshal", "err", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}

func barkDie(format string, a ...any) {
	slog.Error(format, a...)
	os.Exit(1)
}

func barkUsage() {
	fmt.Fprintln(os.Stderr, `usage: whatbark bark [flags] <command> [args]

commands:
  send --to <emp> --kind <dispatch|ask|chat|report> --body <text> [--project <id>]
  send --group <id> --kind dispatch --body <text> [--to <target member>] [--project <id>]
  send --group <id> --kind chat --body <text>
  cancel <task-id> [--reason <text>]
  history <emp-id> [--limit N]
  tasks [--status S] [--employee N]
  decompose --data '{"name":...,"nodes":[{"employee_id":N}],"edges":[{"from":N,"to":M}],"tasks":[{"employee_id":N,"input":"..."}]}'
  status

flags:
  -server URL   platform URL (default from BARK_SERVER or http://127.0.0.1:8080)
  -token STR    bearer token (default from BARK_TOKEN)`)
}

// cmdBark is the CLI used by AI agents (and humans) to talk to the platform.
func cmdBark(args []string) {
	fs := flag.NewFlagSet("bark", flag.ExitOnError)
	serverURL := fs.String("server", envOr("BARK_SERVER", "http://127.0.0.1:8080"), "server URL")
	token := fs.String("token", os.Getenv("BARK_TOKEN"), "bearer token (or set BARK_TOKEN)")
	fs.Parse(args)
	args = fs.Args()
	if len(args) == 0 {
		barkUsage()
		os.Exit(2)
	}
	c := newBarkClient(*serverURL, *token)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "send":
		barkSend(ctx, c, rest)
	case "cancel":
		barkCancel(ctx, c, rest)
	case "history":
		barkHistory(ctx, c, rest)
	case "tasks":
		barkTasks(ctx, c, rest)
	case "decompose":
		barkDecompose(ctx, c, rest)
	case "status":
		barkStatus(ctx, c)
	default:
		barkUsage()
		os.Exit(2)
	}
}

// barkSend: POST /api/messages. The platform returns task_id when a task is
// created (dispatch/ask); the agent records it and works on it via the agent loop.
// S22: --group <id> targets a group thread; with kind=dispatch and --to the
// target member it becomes an in-group task dispatch (201 {message_id,task_id}).
func barkSend(ctx context.Context, c *barkClient, args []string) {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	to := fs.Int("to", 0, "recipient employee id (group dispatch: the target member)")
	kind := fs.String("kind", "dispatch", "message kind (dispatch|ask|chat|report)")
	body := fs.String("body", "", "message body")
	proj := fs.Int("project", 0, "optional project id")
	prio := fs.Int("priority", 0, "task priority 0..99 (higher = more urgent)")
	grp := fs.Int("group", 0, "group id (S22: in-group message; dispatch + to = in-group task dispatch)")
	fs.Parse(args)
	if (*to <= 0 && *grp <= 0) || *body == "" {
		barkUsage()
		os.Exit(2)
	}
	if *prio < 0 || *prio > 99 {
		barkDie("send: priority must be in [0,99]")
	}
	req := map[string]any{"kind": *kind, "body": *body}
	if *to > 0 {
		req["to"] = *to
	}
	if *grp > 0 {
		req["group"] = *grp
	}
	if *proj > 0 {
		req["project_id"] = *proj
	}
	if *prio > 0 {
		req["priority"] = *prio
	}
	var out map[string]any
	if err := c.do(ctx, "POST", "/api/messages", req, &out); err != nil {
		barkDie("send: %v", err)
	}
	printJSON(out)
}

func barkCancel(ctx context.Context, c *barkClient, args []string) {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	reason := fs.String("reason", "", "cancel reason")
	fs.Parse(args)
	if fs.NArg() != 1 {
		barkDie("cancel: usage: cancel <task-id> [--reason text]")
	}
	taskID := fs.Arg(0)
	var out map[string]any
	if err := c.do(ctx, "POST", "/api/tasks/"+taskID+"/cancel", map[string]any{"reason": *reason}, &out); err != nil {
		barkDie("cancel: %v", err)
	}
	printJSON(out)
}

func barkHistory(ctx context.Context, c *barkClient, args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	limit := fs.Int("limit", 50, "max messages")
	fs.Parse(args)
	if fs.NArg() != 1 {
		barkDie("history: usage: history <emp-id> [--limit N]")
	}
	q := "?to=" + fs.Arg(0) + "&limit=" + itoa(*limit)
	var out map[string]any
	if err := c.do(ctx, "GET", "/api/messages"+q, nil, &out); err != nil {
		barkDie("history: %v", err)
	}
	printJSON(out)
}

func barkTasks(ctx context.Context, c *barkClient, args []string) {
	fs := flag.NewFlagSet("tasks", flag.ExitOnError)
	status := fs.String("status", "", "filter by status (queued|running|done|failed|canceled)")
	emp := fs.Int("employee", 0, "filter by employee (non-admin: self only)")
	fs.Parse(args)
	var qs []string
	if *status != "" {
		qs = append(qs, "status="+*status)
	}
	if *emp > 0 {
		qs = append(qs, "employee="+itoa(*emp))
	}
	path := "/api/tasks"
	if len(qs) > 0 {
		path += "?" + strings.Join(qs, "&")
	}
	var out map[string]any
	if err := c.do(ctx, "GET", path, nil, &out); err != nil {
		barkDie("tasks: %v", err)
	}
	printJSON(out)
}

// barkDecompose: POST /api/decompose (V-3). A kind=ai lead submits a workflow
// graph (nodes = employee ids, edges = dependencies) plus optional initial
// tasks as a single JSON document via --data.
func barkDecompose(ctx context.Context, c *barkClient, args []string) {
	fs := flag.NewFlagSet("decompose", flag.ExitOnError)
	data := fs.String("data", "", "JSON: {name, nodes:[{employee_id,x?,y?}], edges:[{from,to}], tasks?:[{employee_id,input}]}")
	fs.Parse(args)
	if *data == "" {
		barkDie("decompose: usage: decompose --data '<json>'")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(*data), &payload); err != nil {
		barkDie("decompose: --data is not valid JSON: %v", err)
	}
	var out map[string]any
	if err := c.do(ctx, "POST", "/api/decompose", payload, &out); err != nil {
		barkDie("decompose: %v", err)
	}
	printJSON(out)
}

func barkStatus(ctx context.Context, c *barkClient) {
	var me map[string]any
	if err := c.do(ctx, "GET", "/api/me", nil, &me); err != nil {
		barkDie("status: %v", err)
	}
	printJSON(me)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
