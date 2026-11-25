package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"whatbark/internal/flow"
	"whatbark/internal/hub"
	"whatbark/internal/store"
)

const adminTok = "admin-test-token"

type env struct {
	srv *httptest.Server
	api *Server
	st  *store.Store
	h   *hub.Hub
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/api-test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	h := hub.New()
	fl := flow.New(st, nil, h.Broadcast)
	s := New(st, fl, h, adminTok)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = st.Close()
	})
	return &env{srv: srv, api: s, st: st, h: h}
}

// makeEmployee creates an employee with a known token and returns (id, token).
func makeEmployee(t *testing.T, e *env, name, kind string, deptID int) (int, string) {
	t.Helper()
	id, err := e.st.CreateEmployee(name, "role", kind, deptID)
	if err != nil {
		t.Fatalf("CreateEmployee %s: %v", name, err)
	}
	tok := "tok-" + name
	sum := sha256.Sum256([]byte(tok))
	if err := e.st.SetTokenHash(id, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("SetTokenHash %s: %v", name, err)
	}
	return id, tok
}

// req performs an HTTP request with optional Bearer token and JSON body.
func req(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(buf) > 0 {
		if err := json.Unmarshal(buf, &out); err != nil {
			t.Fatalf("unmarshal %s %s: %v (body=%s)", method, url, err, buf)
		}
	}
	return resp.StatusCode, out
}

func inbox(t *testing.T, e *env, tok string, empID int) []map[string]any {
	t.Helper()
	code, body := req(t, http.MethodGet, fmt.Sprintf("%s/api/messages?to=%d", e.srv.URL, empID), tok, nil)
	if code != http.StatusOK {
		t.Fatalf("inbox %d: status %d", empID, code)
	}
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

func tasksOf(t *testing.T, e *env, tok string, empID int) []map[string]any {
	t.Helper()
	code, body := req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks?employee=%d", e.srv.URL, empID), tok, nil)
	if code != http.StatusOK {
		t.Fatalf("tasks %d: status %d", empID, code)
	}
	raw, _ := body["tasks"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

// topo builds: A (leader L1) <- B (leader L2, worker W1).
func topo(t *testing.T, e *env) (l1 int, l1tok string, w1 int, w1tok string) {
	t.Helper()
	a, err := e.st.CreateDepartment("A", nil)
	if err != nil {
		t.Fatal(err)
	}
	l1, l1tok = makeEmployee(t, e, "L1", store.KindAI, a)
	if err := e.st.SetDepartmentLeader(a, l1); err != nil {
		t.Fatal(err)
	}
	b, err := e.st.CreateDepartment("B", &a)
	if err != nil {
		t.Fatal(err)
	}
	w1, w1tok = makeEmployee(t, e, "W1", store.KindAI, b)
	l2 := func() int { id, _ := makeEmployee(t, e, "L2", store.KindAI, b); return id }()
	if err := e.st.SetDepartmentLeader(b, l2); err != nil {
		t.Fatal(err)
	}
	return l1, l1tok, w1, w1tok
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	code, _ := req(t, http.MethodGet, e.srv.URL+"/healthz", "", nil)
	if code != http.StatusOK {
		t.Errorf("healthz: got %d want 200", code)
	}
}

func TestStaticFrontend(t *testing.T) {
	e := newEnv(t)
	r, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("static /: got %d want 200", r.StatusCode)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI, a)

	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/me", "", nil); code != http.StatusUnauthorized {
		t.Errorf("no token: got %d want 401", code)
	}
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/me", "wrong-token", nil); code != http.StatusUnauthorized {
		t.Errorf("bad token: got %d want 401", code)
	}
	code, body := req(t, http.MethodGet, e.srv.URL+"/api/me", adminTok, nil)
	if code != http.StatusOK || body["admin"] != true {
		t.Errorf("admin me: code=%d body=%v", code, body)
	}
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/me", tok, nil)
	if code != http.StatusOK || body["admin"] != false {
		t.Errorf("employee me: code=%d body=%v", code, body)
	}
	if got := body["employee_id"]; got != float64(empID) {
		t.Errorf("employee_id = %v want %d", got, empID)
	}
}

func TestAdminOnly(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	_, tok := makeEmployee(t, e, "E1", store.KindAI, a)

	code, _ := req(t, http.MethodPost, e.srv.URL+"/api/departments", tok, map[string]any{"name": "X"})
	if code != http.StatusForbidden {
		t.Errorf("employee create dept: got %d want 403", code)
	}
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/departments", adminTok, map[string]any{"name": "X"})
	if code != http.StatusCreated {
		t.Errorf("admin create dept: got %d want 201", code)
	}
}

func TestSendRoute403(t *testing.T) {
	e := newEnv(t)
	l1, _, _, w1tok := topo(t, e)

	// W1 -> L1 skips a level (W1's leader is L2, not L1): dispatch rejected.
	code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", w1tok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "x"})
	if code != http.StatusForbidden {
		t.Errorf("out-of-topology dispatch: got %d want 403", code)
	}
}

// TestRoutesConsistentWithEnforcement pins GET /api/routes to the same
// Allowed() predicate Flow.Send enforces (S11d: the chat list used to include
// every leader even for non-leader callers, so the web composer offered
// "chat" sends that the flow layer rejected with 403 route-not-allowed).
func TestRoutesConsistentWithEnforcement(t *testing.T) {
	e := newEnv(t)
	a, err := e.st.CreateDepartment("A", nil)
	if err != nil {
		t.Fatal(err)
	}
	l1, l1tok := makeEmployee(t, e, "L1", store.KindAI, a)
	if err := e.st.SetDepartmentLeader(a, l1); err != nil {
		t.Fatal(err)
	}
	b, err := e.st.CreateDepartment("B", &a)
	if err != nil {
		t.Fatal(err)
	}
	l2, _ := makeEmployee(t, e, "L2", store.KindAI, b)
	w1, w1tok := makeEmployee(t, e, "W1", store.KindAI, b)
	if err := e.st.SetDepartmentLeader(b, l2); err != nil {
		t.Fatal(err)
	}

	routesOf := func(empID int) map[string]any {
		t.Helper()
		code, body := req(t, http.MethodGet, fmt.Sprintf("%s/api/routes?employee_id=%d", e.srv.URL, empID), adminTok, nil)
		if code != http.StatusOK {
			t.Fatalf("routes %d: got %d", empID, code)
		}
		return body
	}
	ids := func(v any) []int {
		t.Helper()
		raw, _ := v.([]any)
		out := make([]int, 0, len(raw))
		for _, x := range raw {
			out = append(out, int(x.(float64)))
		}
		return out
	}

	// Non-leader W1: chat list must be empty (chat is leader<->leader only),
	// reporting chain intact in proximity order.
	body := routesOf(w1)
	if got := ids(body["chat"]); len(got) != 0 {
		t.Errorf("W1 chat = %v, want [] (non-leader cannot chat)", got)
	}
	if got := ids(body["reporting"]); !slices.Equal(got, []int{l2, l1}) {
		t.Errorf("W1 reporting = %v, want [%d %d]", got, l2, l1)
	}

	// Leader L2: chat peers are exactly the other leaders, never the worker;
	// dispatch covers its down-chain.
	body = routesOf(l2)
	if got := ids(body["chat"]); !slices.Equal(got, []int{l1}) {
		t.Errorf("L2 chat = %v, want [%d]", got, l1)
	}
	if got := ids(body["dispatch"]); !slices.Equal(got, []int{w1}) {
		t.Errorf("L2 dispatch = %v, want [%d]", got, w1)
	}

	// Endpoint and enforcement agree: exactly the listed pairs send fine.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", w1tok,
		map[string]any{"to": l2, "kind": "chat", "body": "x"}); code != http.StatusForbidden {
		t.Errorf("W1->L2 chat send: got %d, want 403 (matches empty chat list)", code)
	}
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": l2, "kind": "chat", "body": "x"}); code != http.StatusCreated {
		t.Errorf("L1->L2 chat send: got %d, want 201 (listed in L1 chat)", code)
	}
}

// TestFullLoop exercises A4 end to end: seeded project task, agent attach,
// dispatch down the chain, auto-report up, ask a human, human answers,
// and the entry==owner no-auto-reply rule.
func TestFullLoop(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)
	h1, h1tok := makeEmployee(t, e, "H1", store.KindHuman, func() int {
		depts, _ := e.st.ListDepartments()
		return depts[0].ID
	}())

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/projects", adminTok, map[string]any{
		"name": "P1", "brief": "kick off", "entry_employee_id": l1,
	})
	if code != http.StatusCreated {
		t.Fatalf("create project: %d %v", code, body)
	}
	pid := int(body["id"].(float64))

	// 1) L1 has the seeded queued dispatch.
	l1tasks := tasksOf(t, e, l1tok, l1)
	if len(l1tasks) != 1 {
		t.Fatalf("L1 tasks = %d want 1: %v", len(l1tasks), l1tasks)
	}
	seedTask := int(l1tasks[0]["id"].(float64))
	if l1tasks[0]["status"] != store.TaskQueued {
		t.Fatalf("seed task status = %v want queued", l1tasks[0]["status"])
	}

	// 2) L1 attaches as an agent (heartbeat + online).
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/agent/attach", l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("agent attach: got %d", code)
	}

	// 3) L1 starts the seeded task.
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, seedTask), l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("start seed task: got %d", code)
	}

	// 4) L1 dispatches to W1.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "subtask"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch L1->W1: %d %v", code, body)
	}
	subTask := int(body["task_id"].(float64))

	// 4b) The dispatch message carries the back-filled task_id.
	ibx := inbox(t, e, w1tok, w1)
	if len(ibx) != 1 {
		t.Fatalf("W1 inbox after dispatch: got %d messages want 1", len(ibx))
	}
	if tid, ok := ibx[0]["task_id"]; !ok || int(tid.(float64)) != subTask {
		t.Errorf("dispatch message task_id: got %v want %d", ibx[0]["task_id"], subTask)
	}

	// 5) W1 sees the message, starts, finishes.
	if len(inbox(t, e, w1tok, w1)) == 0 {
		t.Fatal("W1 inbox empty after dispatch")
	}
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, subTask), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("W1 start: %d", code)
	}
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, subTask), w1tok,
		map[string]any{"output": "done"})
	if code != http.StatusOK {
		t.Fatalf("W1 finish: %d", code)
	}

	// 6) Auto report lands in L1's inbox (requested_by=L1).
	var gotReport bool
	for _, m := range inbox(t, e, l1tok, l1) {
		if m["kind"] == store.MsgReport && m["task_id"] == float64(subTask) {
			gotReport = true
		}
	}
	if !gotReport {
		t.Error("no auto report to L1 after W1 finished")
	}

	// 7) L1 asks human H1.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": h1, "kind": "ask", "body": "question?"})
	if code != http.StatusCreated {
		t.Fatalf("ask L1->H1: %d %v", code, body)
	}
	askTask := int(body["task_id"].(float64))

	// 8) H1 (human token) finishes the ask task -> answer auto-replies to L1.
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, askTask), h1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("H1 start: %d", code)
	}
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, askTask), h1tok,
		map[string]any{"output": "the answer"})
	if code != http.StatusOK {
		t.Fatalf("H1 finish: %d", code)
	}
	var gotAnswer bool
	for _, m := range inbox(t, e, l1tok, l1) {
		if m["kind"] == store.MsgAnswer && m["body"] == "the answer" {
			gotAnswer = true
		}
	}
	if !gotAnswer {
		t.Error("no answer to L1 after H1 finished")
	}

	// 9) Boss dispatch on project P1 where entry==owner (L1): no auto report.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "boss work", "project_id": pid})
	if code != http.StatusCreated {
		t.Fatalf("boss dispatch: %d %v", code, body)
	}
	bossTask := int(body["task_id"].(float64))
	req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, bossTask), l1tok, nil)
	req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, bossTask), l1tok,
		map[string]any{"output": "boss done"})
	var foundReport bool
	for _, m := range inbox(t, e, l1tok, l1) {
		if m["kind"] == store.MsgReport && m["task_id"] == float64(bossTask) {
			foundReport = true
		}
	}
	if foundReport {
		t.Error("unexpected auto report when project entry == task owner")
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, _, w1tok := topo(t, e)

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/projects", adminTok, map[string]any{
		"name": "P", "brief": "b", "entry_employee_id": l1,
	})
	if code != http.StatusCreated {
		t.Fatalf("create project: %d %v", code, body)
	}
	taskID := int(tasksOf(t, e, l1tok, l1)[0]["id"].(float64))

	// Unrelated employee cannot cancel L1's task.
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/cancel", e.srv.URL, taskID), w1tok,
		map[string]any{"reason": "nope"})
	if code != http.StatusForbidden {
		t.Errorf("unrelated cancel: got %d want 403", code)
	}

	// Owner can cancel own queued task.
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/cancel", e.srv.URL, taskID), l1tok,
		map[string]any{"reason": "stale"})
	if code != http.StatusOK {
		t.Errorf("owner cancel: got %d", code)
	}
	tasks, err := e.st.ListTasksByEmployee(l1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].Status != store.TaskCanceled {
		t.Errorf("task status = %s want canceled", tasks[0].Status)
	}
}

func TestAgentGuards(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, _, w1tok := topo(t, e)
	_, hTok := func() (int, string) {
		depts, _ := e.st.ListDepartments()
		return makeEmployee(t, e, "H1", store.KindHuman, depts[0].ID)
	}()

	// Admin token may not attach as an agent.
	code, _ := req(t, http.MethodPost, e.srv.URL+"/api/agent/attach", adminTok, nil)
	if code != http.StatusForbidden {
		t.Errorf("admin attach: got %d want 403", code)
	}
	// Human token may not attach as an agent.
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/agent/attach", hTok, nil)
	if code != http.StatusForbidden {
		t.Errorf("human attach: got %d want 403", code)
	}
	// AI token attaches fine.
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/agent/attach", l1tok, nil)
	if code != http.StatusOK {
		t.Errorf("ai attach: got %d want 200", code)
	}

	// Agent output restricted to the task owner.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/projects", adminTok, map[string]any{
		"name": "P", "brief": "b", "entry_employee_id": l1,
	})
	if code != http.StatusCreated {
		t.Fatalf("create project: %d %v", code, body)
	}
	taskID := int(tasksOf(t, e, l1tok, l1)[0]["id"].(float64))
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/agent/output", w1tok,
		map[string]any{"task_id": taskID, "line": "x"})
	if code != http.StatusForbidden {
		t.Errorf("other-agent output: got %d want 403", code)
	}
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/agent/output", l1tok,
		map[string]any{"task_id": taskID, "line": "x"})
	if code != http.StatusOK {
		t.Errorf("owner agent output: got %d want 200", code)
	}
}

// TestWSEndpoint verifies live event delivery (A7): task.state and
// message.new frames reach a connected WebSocket client.
func TestWSEndpoint(t *testing.T) {
	e := newEnv(t)
	l1, _, _, _ := topo(t, e)

	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws?token=" + adminTok
	ws, err := websocket.Dial(wsURL, "", e.srv.URL)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()

	deadline := time.Now().Add(5 * time.Second)
	for e.h.Clients() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if e.h.Clients() == 0 {
		t.Fatal("ws client not registered on hub")
	}

	code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "hi"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: %d", code)
	}

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var sawTaskState, sawMessageNew bool
	buf := make([]byte, 4096)
	for !(sawTaskState && sawMessageNew) {
		n, err := ws.Read(buf)
		if err != nil {
			t.Fatalf("ws read: %v (sawTaskState=%v sawMessageNew=%v)", err, sawTaskState, sawMessageNew)
		}
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(buf[:n], &ev); err == nil {
			switch ev.Type {
			case "task.state":
				sawTaskState = true
			case "message.new":
				sawMessageNew = true
			}
		}
	}
	if !sawTaskState || !sawMessageNew {
		t.Errorf("ws events: task.state=%v message.new=%v", sawTaskState, sawMessageNew)
	}
}

// TestOfflineReaping verifies heartbeats: a stale agent is reaped.
func TestOfflineReaping(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, _, _ := topo(t, e)

	code, _ := req(t, http.MethodPost, e.srv.URL+"/api/agent/attach", l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("attach: %d", code)
	}
	e.api.offlineAt = 50 * time.Millisecond
	time.Sleep(150 * time.Millisecond)
	e.api.reapOffline()

	e.api.mu.Lock()
	_, online := e.api.online[l1]
	e.api.mu.Unlock()
	if online {
		t.Error("stale agent still marked online after reap")
	}

	emp, err := e.st.GetEmployee(l1)
	if err != nil {
		t.Fatal(err)
	}
	if emp.LastSeenAt == nil {
		t.Error("last_seen_at not persisted on attach")
	}
}

// TestMessageReadReceipts (S19-B): viewer-sent messages carry a "read" bool.
// 1:1: driven by the peer's read cursor; group: driven by any OTHER member's
// cursor. The viewer's own cursor never counts, and system messages carry no
// read field.
func TestMessageReadReceipts(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	// 1:1 thread l1<->w1: l1 dispatches, w1 reports back.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "do the thing"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: %d %v", code, body)
	}
	l1msg := int(body["message_id"].(float64))

	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", w1tok,
		map[string]any{"to": l1, "kind": "report", "body": "done"})
	if code != http.StatusCreated {
		t.Fatalf("report: %d", code)
	}

	threadOf := func() map[int]map[string]any {
		t.Helper()
		code, body := req(t, http.MethodGet,
			fmt.Sprintf("%s/api/messages?with=%d", e.srv.URL, w1), l1tok, nil)
		if code != http.StatusOK {
			t.Fatalf("list thread: %d %v", code, body)
		}
		out := map[int]map[string]any{}
		for _, m := range body["messages"].([]any) {
			row := m.(map[string]any)
			out[int(row["id"].(float64))] = row
		}
		return out
	}

	// Initially w1 has not read anything -> l1's message is unread.
	if got := threadOf()[l1msg]["read"]; got != false {
		t.Fatalf("initial read = %v, want false", got)
	}

	// l1 marking its own thread read advances l1's cursor only and must NOT
	// flip l1's own message to read.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages/read", l1tok,
		map[string]any{"with": w1}); code != http.StatusOK {
		t.Fatalf("l1 mark read: %d", code)
	}
	if got := threadOf()[l1msg]["read"]; got != false {
		t.Fatalf("after own mark read: read=%v, want false (own cursor excluded)", got)
	}

	// w1 marking the thread read -> l1's message flips to read.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages/read", w1tok,
		map[string]any{"with": l1}); code != http.StatusOK {
		t.Fatalf("w1 mark read: %d", code)
	}
	rows := threadOf()
	if got := rows[l1msg]["read"]; got != true {
		t.Fatalf("after w1 mark read: read=%v, want true", got)
	}
	// w1's own message must not carry a read field in l1's view.
	for id, row := range rows {
		if row["from_id"] == float64(float64(w1)) && row["id"] == float64(id) && id != l1msg {
			if _, has := row["read"]; has {
				t.Fatalf("non-viewer-sent msg %d carries read field: %v", id, row)
			}
		}
	}

	// Group: l1 creates a group with w1; l1's chat stays unread until w1
	// (another member) marks the group read. l1's own cursor never counts.
	gid := makeGroup(t, e, l1tok, "read-g", []int{w1})
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gid, "kind": "chat", "body": "hello group"})
	if code != http.StatusCreated {
		t.Fatalf("group chat: %d %v", code, body)
	}
	gmsg := int(body["message_id"].(float64))

	glist := func() map[int]map[string]any {
		t.Helper()
		code, body := req(t, http.MethodGet,
			fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gid), l1tok, nil)
		if code != http.StatusOK {
			t.Fatalf("list group: %d %v", code, body)
		}
		out := map[int]map[string]any{}
		for _, m := range body["messages"].([]any) {
			row := m.(map[string]any)
			out[int(row["id"].(float64))] = row
		}
		return out
	}

	rows = glist()
	for id, row := range rows {
		if row["kind"] == "system" {
			if _, has := row["read"]; has {
				t.Fatalf("system msg %d carries read field: %v", id, row)
			}
		}
	}
	if got := rows[gmsg]["read"]; got != false {
		t.Fatalf("group msg before reads: read=%v, want false", got)
	}

	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages/read", l1tok,
		map[string]any{"group": gid}); code != http.StatusOK {
		t.Fatalf("l1 group mark read: %d", code)
	}
	if got := glist()[gmsg]["read"]; got != false {
		t.Fatalf("after l1's own group mark read: read=%v, want false", got)
	}

	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages/read", w1tok,
		map[string]any{"group": gid}); code != http.StatusOK {
		t.Fatalf("w1 group mark read: %d", code)
	}
	if got := glist()[gmsg]["read"]; got != true {
		t.Fatalf("after w1 group mark read: read=%v, want true", got)
	}
}

// TestWSMessageReadEvent (S19-B): marking a 1:1 thread read broadcasts a
// message.read frame {reader, peer, up_to} to connected clients.
func TestWSMessageReadEvent(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws?token=" + adminTok
	ws, err := websocket.Dial(wsURL, "", e.srv.URL)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()

	deadline := time.Now().Add(5 * time.Second)
	for e.h.Clients() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if e.h.Clients() == 0 {
		t.Fatal("ws client not registered on hub")
	}

	// l1 sends a message so w1 has something to advance the cursor to.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "read me"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: %d %v", code, body)
	}
	wantUpTo := int(body["message_id"].(float64))

	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages/read", w1tok,
		map[string]any{"with": l1}); code != http.StatusOK {
		t.Fatalf("w1 mark read: %d", code)
	}

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	for {
		n, err := ws.Read(buf)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var ev struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(buf[:n], &ev); err != nil {
			continue
		}
		if ev.Type != "message.read" {
			continue
		}
		if int(ev.Payload["reader"].(float64)) != w1 {
			t.Errorf("reader = %v, want %d", ev.Payload["reader"], w1)
		}
		if int(ev.Payload["peer"].(float64)) != l1 {
			t.Errorf("peer = %v, want %d", ev.Payload["peer"], l1)
		}
		if int(ev.Payload["up_to"].(float64)) != wantUpTo {
			t.Errorf("up_to = %v, want %d", ev.Payload["up_to"], wantUpTo)
		}
		return
	}
}
