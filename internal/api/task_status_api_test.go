package api

import (
	"fmt"
	"net/http"
	"testing"

	"whatbark/internal/store"
)

// groupRows returns the caller's view of a group thread keyed by message id.
func groupRows(t *testing.T, e *env, tok string, gid int) map[int]map[string]any {
	t.Helper()
	code, body := req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gid), tok, nil)
	if code != http.StatusOK {
		t.Fatalf("group thread %d: got %d", gid, code)
	}
	out := map[int]map[string]any{}
	for _, m := range body["messages"].([]any) {
		row := m.(map[string]any)
		out[int(row["id"].(float64))] = row
	}
	return out
}

// rowsWithTask collects every row of a thread view that carries task_id.
func rowsWithTask(rows map[int]map[string]any, taskID int) []map[string]any {
	out := []map[string]any{}
	for _, r := range rows {
		if tid, ok := r["task_id"]; ok && int(tid.(float64)) == taskID {
			out = append(out, r)
		}
	}
	return out
}

// TestGroupThreadTaskStatus (S28): in a group thread, every row carrying a
// task_id is annotated with the task's live status for all members,
// including non-owners. Follows a boss-dispatched task through
// queued -> running -> done; the auto report row (same task link) shows the
// same live status. Rows without a task_id never carry task_status, and the
// non-owner's own task list stays self-scoped.
func TestGroupThreadTaskStatus(t *testing.T) {
	e := newEnv(t)
	_, _, w1, w1tok := topo(t, e)
	deptC, err := e.st.CreateDepartment("C", nil)
	if err != nil {
		t.Fatal(err)
	}
	w2, w2tok := makeEmployee(t, e, "W2", store.KindAI, deptC)

	gid := makeGroup(t, e, adminTok, "s28-live", []int{w1, w2})

	// Boss dispatches w1 inside the group: 201 + task_id (queued).
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gid, "to": w1, "kind": "dispatch", "body": "ship it"})
	if code != http.StatusCreated {
		t.Fatalf("group dispatch: got %d, want 201 (body %v)", code, body)
	}
	taskID := int(body["task_id"].(float64))

	// w2 (non-owner) sees the dispatch row with task_status=queued, and the
	// self-scoped task list stays clean.
	rows := groupRows(t, e, w2tok, gid)
	linking := rowsWithTask(rows, taskID)
	if len(linking) != 1 {
		t.Fatalf("rows linking task %d = %d, want 1 (dispatch only): %v", taskID, len(linking), rows)
	}
	if st := linking[0]["task_status"]; st != store.TaskQueued {
		t.Fatalf("queued: task_status = %v, want %q", st, store.TaskQueued)
	}
	for _, r := range rows {
		if _, hasTask := r["task_id"]; !hasTask {
			if _, hasStatus := r["task_status"]; hasStatus {
				t.Fatalf("row without task_id carries task_status: %v", r)
			}
		}
	}
	for _, tk := range tasksOf(t, e, w2tok, w2) {
		if int(tk["id"].(float64)) == taskID {
			t.Fatalf("task %d leaked into w2's self-scoped task list: %v", taskID, tk)
		}
	}

	// Boss (admin) starts the task: w2's refetch shows running.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, taskID), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("boss start: got %d, want 200", code)
	}
	for _, r := range rowsWithTask(groupRows(t, e, w2tok, gid), taskID) {
		if st := r["task_status"]; st != store.TaskRunning {
			t.Fatalf("running: task_status = %v, want %q", st, store.TaskRunning)
		}
	}

	// Owner finishes: done. The auto report row (kind=report, same task link)
	// appears in the group thread and shows the same live status.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, taskID), w1tok,
		map[string]any{"output": "x"})
	if code != http.StatusOK {
		t.Fatalf("w1 finish: got %d, want 200", code)
	}
	rows = groupRows(t, e, w2tok, gid)
	linking = rowsWithTask(rows, taskID)
	if len(linking) < 2 {
		t.Fatalf("rows linking task %d after finish = %d, want >= 2 (dispatch + report): %v",
			taskID, len(linking), rows)
	}
	sawReport := false
	for _, r := range linking {
		if st := r["task_status"]; st != store.TaskDone {
			t.Fatalf("done: task_status = %v, want %q (row %v)", st, store.TaskDone, r)
		}
		if r["kind"] == store.MsgReport {
			sawReport = true
		}
	}
	if !sawReport {
		t.Fatalf("no report row linking task %d in group thread: %v", taskID, rows)
	}
}

// TestOneToOneThreadNoTaskStatus (S28): the 1:1 (?with=) branch is unchanged —
// dispatch rows carry task_id but no task_status key, for both sides of the
// conversation.
func TestOneToOneThreadNoTaskStatus(t *testing.T) {
	e := newEnv(t)
	_, _, w1, w1tok := topo(t, e)

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "solo job"})
	if code != http.StatusCreated {
		t.Fatalf("1:1 dispatch: got %d, want 201 (body %v)", code, body)
	}
	taskID := int(body["task_id"].(float64))

	threadOf := func(tok string, peer int) map[int]map[string]any {
		t.Helper()
		code, body := req(t, http.MethodGet,
			fmt.Sprintf("%s/api/messages?with=%d", e.srv.URL, peer), tok, nil)
		if code != http.StatusOK {
			t.Fatalf("thread with=%d: got %d", peer, code)
		}
		out := map[int]map[string]any{}
		for _, m := range body["messages"].([]any) {
			row := m.(map[string]any)
			out[int(row["id"].(float64))] = row
		}
		return out
	}

	for name, view := range map[string]map[int]map[string]any{
		"boss view": threadOf(adminTok, w1),
		"w1 view":   threadOf(w1tok, 0),
	} {
		found := false
		for _, r := range view {
			if tid, ok := r["task_id"]; ok && int(tid.(float64)) == taskID {
				found = true
			}
			if _, has := r["task_status"]; has {
				t.Errorf("%s: 1:1 row %v carries task_status, want none", name, r)
			}
		}
		if !found {
			t.Errorf("%s: task %d missing from 1:1 thread: %v", name, taskID, view)
		}
	}
}

// TestGroupThreadNonMember403 (S28 regression): a non-member employee is
// still rejected from a group thread by GET /api/messages?group=G.
func TestGroupThreadNonMember403(t *testing.T) {
	e := newEnv(t)
	_, _, w1, _ := topo(t, e)
	gid := makeGroup(t, e, adminTok, "s28-403", []int{w1})

	deptD, err := e.st.CreateDepartment("D", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, z1tok := makeEmployee(t, e, "Z1", store.KindAI, deptD)

	code, _ := req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gid), z1tok, nil)
	if code != http.StatusForbidden {
		t.Errorf("non-member group thread: got %d, want 403", code)
	}
}
