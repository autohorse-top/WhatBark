package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestTaskReassignEndpoint (S27): POST /api/tasks/{id}/reassign
// Verifies response codes 200/400/403/404/409, body shape, audit note,
// and that the old owner is locked out after reassignment.
func TestTaskReassignEndpoint(t *testing.T) {
	e := newEnv(t)
	_, _, w1, w1tok := topo(t, e)

	// Create two extra employees for target / unauthorized use cases.
	deptA, err := e.st.ListDepartments()
	if err != nil {
		t.Fatalf("list depts: %v", err)
	}
	aID := deptA[0].ID
	t1, t1tok := makeEmployee(t, e, "T1", "ai", aID)
	_, x1tok := makeEmployee(t, e, "X1", "ai", aID)

	// --- Setup: boss dispatches a task to w1 ---
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "do work"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: got %d %v", code, body)
	}
	taskID := int(body["task_id"].(float64))

	// --- 1. Unknown task -> 404 ---
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/tasks/999999/reassign", adminTok,
		map[string]any{"to": t1})
	if code != http.StatusNotFound {
		t.Errorf("unknown task reassign: got %d, want 404", code)
	}

	// --- 2. Bad id -> 400 ---
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/tasks/abc/reassign", adminTok,
		map[string]any{"to": t1})
	if code != http.StatusBadRequest {
		t.Errorf("bad id reassign: got %d, want 400", code)
	}

	// --- 3. Unauthorized employee -> 403 ---
	// X1 is unrelated to w1's chain, not requester (0), not owner.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/reassign", e.srv.URL, taskID), x1tok,
		map[string]any{"to": t1})
	if code != http.StatusForbidden {
		t.Errorf("unauthorized reassign: got %d, want 403", code)
	}

	// --- 4. Same owner -> 400 ---
	code, body = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/reassign", e.srv.URL, taskID), adminTok,
		map[string]any{"to": w1})
	if code != http.StatusBadRequest {
		t.Errorf("same-owner reassign: got %d, want 400", code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "no-op") {
		t.Errorf("same-owner error = %q, want no-op message", msg)
	}

	// --- 5. Unknown employee target -> 404 ---
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/reassign", e.srv.URL, taskID), adminTok,
		map[string]any{"to": 999999})
	if code != http.StatusNotFound {
		t.Errorf("unknown target reassign: got %d, want 404", code)
	}

	// --- 6. Valid reassign (queued) -> 200 ---
	code, body = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/reassign", e.srv.URL, taskID), adminTok,
		map[string]any{"to": t1, "reason": "handoff"})
	if code != http.StatusOK {
		t.Fatalf("reassign queued: got %d %v", code, body)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["status"] != "queued" {
		t.Errorf("status = %v, want queued", body["status"])
	}
	if int(body["employee_id"].(float64)) != t1 {
		t.Errorf("employee_id = %v, want %d", body["employee_id"], t1)
	}

	// Verify output contains the audit note.
	code, row := req(t, http.MethodGet,
		fmt.Sprintf("%s/api/tasks/%d", e.srv.URL, taskID), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("get task: got %d", code)
	}
	out, _ := row["output"].(string)
	if !strings.Contains(out, "reassigned") {
		t.Errorf("output = %q, want 'reassigned' note", out)
	}
	if !strings.Contains(out, "handoff") {
		t.Errorf("output = %q, want reason 'handoff'", out)
	}

	// --- 7. Old owner cannot finish after reassign -> 403 ---
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, taskID), w1tok,
		map[string]any{"output": "should fail"})
	if code != http.StatusForbidden {
		t.Errorf("old owner finish: got %d, want 403", code)
	}

	// --- 8. New owner can start + finish -> 200 ---
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, taskID), t1tok, nil)
	if code != http.StatusOK {
		t.Errorf("new owner start: got %d, want 200", code)
	}
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, taskID), t1tok,
		map[string]any{"output": "done"})
	if code != http.StatusOK {
		t.Errorf("new owner finish: got %d, want 200", code)
	}

	// --- 9. Reassign a done task -> 409 ---
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/reassign", e.srv.URL, taskID), adminTok,
		map[string]any{"to": w1})
	if code != http.StatusConflict {
		t.Errorf("reassign done task: got %d, want 409", code)
	}

	// --- 10. Reassign a canceled task -> 200 ---
	// Create second task for w1, cancel it, then reassign.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "second"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch 2: got %d %v", code, body)
	}
	taskID2 := int(body["task_id"].(float64))
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/cancel", e.srv.URL, taskID2), w1tok,
		map[string]any{"reason": "not needed"})
	if code != http.StatusOK {
		t.Fatalf("cancel: got %d", code)
	}
	code, body = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/reassign", e.srv.URL, taskID2), adminTok,
		map[string]any{"to": t1})
	if code != http.StatusOK {
		t.Fatalf("reassign canceled task: got %d %v", code, body)
	}
	if body["status"] != "queued" {
		t.Errorf("status = %v, want queued", body["status"])
	}
}
