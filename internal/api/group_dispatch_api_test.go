package api

import (
	"fmt"
	"net/http"
	"testing"

	"whatbark/internal/store"
)

// TestGroupDispatchAPI (S22): dispatching a task inside a group thread.
// Covers the 201 happy path (boss + leader), the full 403/400 matrix,
// open_tasks on the group conversation row, and the report landing back in
// the group thread on task completion.
func TestGroupDispatchAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)
	// c1 leads a root-level department sibling to A (l1 leads A): c1 -> l1
	// is a lateral route (no down edge), so it must be rejected.
	deptC, err := e.st.CreateDepartment("C", nil)
	if err != nil {
		t.Fatal(err)
	}
	c1, c1tok := makeEmployee(t, e, "C1", store.KindAI, deptC)
	if err := e.st.SetDepartmentLeader(deptC, c1); err != nil {
		t.Fatal(err)
	}

	// Group A: boss-created, members {0, l1, w1}.
	gidA := makeGroup(t, e, adminTok, "s22-a", []int{l1, w1})

	// 1) Boss dispatches w1 inside the group: 201 {message_id, task_id}.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gidA, "to": w1, "kind": "dispatch", "body": "ship it"})
	if code != http.StatusCreated {
		t.Fatalf("boss group dispatch: got %d, want 201 (body %v)", code, body)
	}
	msgID := int(body["message_id"].(float64))
	taskID := int(body["task_id"].(float64))
	if msgID == 0 || taskID == 0 {
		t.Fatalf("201 body = %v, want nonzero message_id and task_id", body)
	}

	// 2) Task: employee=w1, requested_by=0 (boss), group_id set, queued.
	task, err := e.st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.EmpID != w1 || task.RequestedBy != 0 || task.Status != store.TaskQueued {
		t.Fatalf("task = %+v, want emp=%d requested_by=0 status=queued", task, w1)
	}
	if task.GroupID == nil || *task.GroupID != gidA {
		t.Fatalf("task.group_id = %v, want %d", task.GroupID, gidA)
	}

	// 3) The group message: kind=dispatch, task_id back-filled.
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gidA), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("group thread: got %d", code)
	}
	msgs, _ := body["messages"].([]any)
	var found bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["id"] == float64(msgID) {
			found = true
			if mm["kind"] != "dispatch" || mm["task_id"] != float64(taskID) {
				t.Errorf("dispatch message = %v, want kind=dispatch task_id=%d", mm, taskID)
			}
		}
	}
	if !found {
		t.Fatalf("dispatch message %d not in group thread: %v", msgID, msgs)
	}

	// 4) Group conversation row: open_tasks 0 -> 1.
	gc, ok := convOf(t, conversations(t, e, w1tok), -(gidA + 1))
	if !ok {
		t.Fatalf("group %d missing from w1 conversations", gidA)
	}
	if got := convInt(gc, "open_tasks"); got != 1 {
		t.Fatalf("group open_tasks = %d, want 1", got)
	}

	// 5) Leader-in-group dispatch (l1 -> w1 is a legal down-chain route): 201.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gidA, "to": w1, "kind": "dispatch", "body": "lead job"})
	if code != http.StatusCreated {
		t.Fatalf("leader group dispatch: got %d, want 201 (body %v)", code, body)
	}
	leadTaskID := int(body["task_id"].(float64))

	// 6) Negative matrix on group A.
	//    non-member sender -> 403
	x1, x1tok := makeEmployee(t, e, "X1", store.KindAI, deptC)
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", x1tok,
		map[string]any{"group": gidA, "to": w1, "kind": "dispatch", "body": "x"})
	if code != http.StatusForbidden {
		t.Errorf("non-member dispatch: got %d, want 403", code)
	}
	//    boss targeting a non-member -> 400
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gidA, "to": x1, "kind": "dispatch", "body": "x"})
	if code != http.StatusBadRequest {
		t.Errorf("boss dispatch to non-member target: got %d, want 400", code)
	}
	//    self dispatch -> 400
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gidA, "to": l1, "kind": "dispatch", "body": "x"})
	if code != http.StatusBadRequest {
		t.Errorf("self dispatch: got %d, want 400", code)
	}
	//    dispatch without to -> 400
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gidA, "kind": "dispatch", "body": "x"})
	if code != http.StatusBadRequest {
		t.Errorf("dispatch without to: got %d, want 400", code)
	}
	//    kind=ask in a group -> 400
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gidA, "to": w1, "kind": "ask", "body": "x"})
	if code != http.StatusBadRequest {
		t.Errorf("group kind=ask: got %d, want 400", code)
	}
	//    S29: unknown project_id in a group -> 400 (valid project_id is
	//    accepted now; see TestGroupDispatchProject)
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gidA, "to": w1, "kind": "dispatch", "body": "x", "project_id": 99999})
	if code != http.StatusBadRequest {
		t.Errorf("group dispatch with unknown project_id: got %d, want 400", code)
	}
	//    chat with to -> 400 (S14 shape)
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gidA, "to": w1, "kind": "chat", "body": "x"})
	if code != http.StatusBadRequest {
		t.Errorf("group chat with to: got %d, want 400", code)
	}

	// 7) Lateral dispatch in an employee-created group -> 403.
	//    Group B: l1-created, members {l1, c1}; c1 -> l1 is lateral (no
	//    down edge between the two root-level leaders).
	gidB := makeGroup(t, e, l1tok, "s22-b", []int{c1})
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", c1tok,
		map[string]any{"group": gidB, "to": l1, "kind": "dispatch", "body": "x"})
	if code != http.StatusForbidden {
		t.Errorf("lateral in-group dispatch: got %d, want 403", code)
	}

	// 8) Group chat still works (S14 regression): 201, no task_id.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gidA, "kind": "chat", "body": "hi"})
	if code != http.StatusCreated {
		t.Fatalf("group chat: got %d, want 201 (body %v)", code, body)
	}
	if _, hasTask := body["task_id"]; hasTask {
		t.Errorf("group chat response must not carry task_id: %v", body)
	}

	// 9) w1 finishes its boss-dispatched task: report lands in the group
	//    thread (from=w1, task_id linked); open_tasks drops 1 -> ... and
	//    reaches 0 once the leader task is done too.
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, taskID), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("start task: got %d", code)
	}
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, taskID), w1tok,
		map[string]any{"output": "shipped"})
	if code != http.StatusOK {
		t.Fatalf("finish task: got %d", code)
	}
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gidA), w1tok, nil)
	msgs, _ = body["messages"].([]any)
	var report bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["kind"] == "report" && mm["task_id"] == float64(taskID) {
			report = true
			if mm["from_id"] != float64(w1) {
				t.Errorf("report from_id = %v, want %d", mm["from_id"], w1)
			}
		}
	}
	if !report {
		t.Errorf("no report message with task_id %d in group thread: %v", taskID, msgs)
	}
	if gc, ok := convOf(t, conversations(t, e, w1tok), -(gidA + 1)); ok {
		if got := convInt(gc, "open_tasks"); got != 1 {
			t.Errorf("group open_tasks after first finish = %d, want 1 (leader task still open)", got)
		}
	}
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, leadTaskID), w1tok, nil)
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, leadTaskID), w1tok,
		map[string]any{"output": "lead done"})
	if code != http.StatusOK {
		t.Fatalf("finish leader task: got %d", code)
	}
	if gc, ok := convOf(t, conversations(t, e, w1tok), -(gidA + 1)); ok {
		if got := convInt(gc, "open_tasks"); got != 0 {
			t.Errorf("group open_tasks after all done = %d, want 0", got)
		}
	}
}
