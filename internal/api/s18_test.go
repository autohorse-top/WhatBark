package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestSendMessagePriority (S18): priority 0..99 is accepted and exposed on
// task rows; out-of-range values are rejected with 400; absent means 0.
func TestSendMessagePriority(t *testing.T) {
	e := newEnv(t)
	l1, _, _, _ := topo(t, e)

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "hi", "priority": 7})
	if code != http.StatusCreated {
		t.Fatalf("send priority 7: got %d %v", code, body)
	}
	taskID := int(body["task_id"].(float64))
	code, row := req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks/%d", e.srv.URL, taskID), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("get task: got %d", code)
	}
	if got := int(row["priority"].(float64)); got != 7 {
		t.Errorf("task priority = %d, want 7", got)
	}

	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "plain"})
	if code != http.StatusCreated {
		t.Fatalf("send without priority: got %d %v", code, body)
	}
	taskID = int(body["task_id"].(float64))
	code, row = req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks/%d", e.srv.URL, taskID), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("get task: got %d", code)
	}
	if got := int(row["priority"].(float64)); got != 0 {
		t.Errorf("default priority = %d, want 0", got)
	}

	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "hot", "priority": 100})
	if code != http.StatusBadRequest {
		t.Errorf("priority 100: got %d, want 400", code)
	}
}

// TestTaskPreemptEndpoint (S18): POST /api/tasks/{id}/preempt requeues a
// running task with an audit note; wrong state -> 409, unknown -> 404,
// non-owner non-admin -> 403.
func TestTaskPreemptEndpoint(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "long work"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: got %d %v", code, body)
	}
	taskID := int(body["task_id"].(float64))
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, taskID), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("start: got %d", code)
	}

	// Owner may preempt.
	code, body = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/preempt", e.srv.URL, taskID), w1tok,
		map[string]any{"reason": "task 9 (priority 9)"})
	if code != http.StatusOK {
		t.Fatalf("preempt: got %d %v", code, body)
	}
	code, row := req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks/%d", e.srv.URL, taskID), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("get task: got %d", code)
	}
	if row["status"] != "queued" {
		t.Errorf("status = %v, want queued", row["status"])
	}
	if out, _ := row["output"].(string); !strings.HasPrefix(out, "preempted:") {
		t.Errorf("output = %q, want preempt audit note", out)
	}

	// Already queued -> 409.
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/preempt", e.srv.URL, taskID), w1tok, nil)
	if code != http.StatusConflict {
		t.Errorf("preempt queued task: got %d, want 409", code)
	}

	// Unknown task -> 404.
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/tasks/999999/preempt", w1tok, nil)
	if code != http.StatusNotFound {
		t.Errorf("preempt missing task: got %d, want 404", code)
	}

	// Non-owner, non-admin -> 403.
	startTask := func() int {
		c, b := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
			map[string]any{"to": w1, "kind": "dispatch", "body": "second"})
		if c != http.StatusCreated {
			t.Fatalf("dispatch: got %d %v", c, b)
		}
		id := int(b["task_id"].(float64))
		req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, id), w1tok, nil)
		return id
	}
	id2 := startTask()
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/preempt", e.srv.URL, id2), l1tok, nil)
	if code != http.StatusForbidden {
		t.Errorf("preempt by non-owner: got %d, want 403", code)
	}
	// Admin may preempt any task.
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/preempt", e.srv.URL, id2), adminTok, nil)
	if code != http.StatusOK {
		t.Errorf("admin preempt: got %d, want 200", code)
	}
}

// TestAgentAttachInitialRequeue (S18): an initial attach requeues orphaned
// running tasks and reports the count; a plain heartbeat must not.
func TestAgentAttachInitialRequeue(t *testing.T) {
	e := newEnv(t)
	_, _, w1, w1tok := topo(t, e)

	// Make a running task the way an agent would.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "orphan"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: got %d %v", code, body)
	}
	taskID := int(body["task_id"].(float64))
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, taskID), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("start: got %d", code)
	}

	// Heartbeat (empty body): must NOT requeue.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/agent/attach", w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("heartbeat attach: got %d %v", code, body)
	}
	if _, hadRequeued := body["requeued"]; hadRequeued {
		t.Errorf("heartbeat attach reported requeued=%v, want no requeue on heartbeat", body["requeued"])
	}
	code, row := req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks/%d", e.srv.URL, taskID), adminTok, nil)
	if code != http.StatusOK || row["status"] != "running" {
		t.Fatalf("task after heartbeat = %v, want running", row)
	}

	// Initial attach: requeues the running task with an audit note.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/agent/attach", w1tok,
		map[string]any{"initial": true})
	if code != http.StatusOK {
		t.Fatalf("initial attach: got %d %v", code, body)
	}
	if got := int(body["requeued"].(float64)); got != 1 {
		t.Errorf("requeued = %d, want 1", got)
	}
	code, row = req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks/%d", e.srv.URL, taskID), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("get task: got %d", code)
	}
	if row["status"] != "queued" {
		t.Errorf("status = %v, want queued after initial attach", row["status"])
	}
	if out, _ := row["output"].(string); !strings.Contains(out, "agent reattached") {
		t.Errorf("output = %q, want requeue audit note", out)
	}
}

// TestListTasksStatusFilter (S18): agents use ?employee=N&status=queued to
// find their pending work; without the filter all statuses are returned.
func TestListTasksStatusFilter(t *testing.T) {
	e := newEnv(t)
	_, _, w1, w1tok := topo(t, e)

	mk := func() int {
		c, b := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
			map[string]any{"to": w1, "kind": "dispatch", "body": "x"})
		if c != http.StatusCreated {
			t.Fatalf("dispatch: got %d %v", c, b)
		}
		return int(b["task_id"].(float64))
	}
	queued := mk()
	running := mk()
	code, _ := req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, running), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("start: got %d", code)
	}

	byStatus := func(status string) []int {
		t.Helper()
		q := fmt.Sprintf("%s/api/tasks?employee=%d", e.srv.URL, w1)
		if status != "" {
			q += "&status=" + status
		}
		code, body := req(t, http.MethodGet, q, w1tok, nil)
		if code != http.StatusOK {
			t.Fatalf("list tasks: got %d", code)
		}
		var ids []int
		for _, m := range body["tasks"].([]any) {
			ids = append(ids, int(m.(map[string]any)["id"].(float64)))
		}
		return ids
	}

	if got := byStatus("queued"); len(got) != 1 || got[0] != queued {
		t.Errorf("status=queued = %v, want [%d]", got, queued)
	}
	if got := byStatus("running"); len(got) != 1 || got[0] != running {
		t.Errorf("status=running = %v, want [%d]", got, running)
	}
	if got := byStatus(""); len(got) != 2 {
		t.Errorf("no filter = %v, want both tasks", got)
	}
}
