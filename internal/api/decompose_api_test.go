package api

import (
	"fmt"
	"net/http"
	"testing"

	"whatbark/internal/store"
)

// TestDecompose (V-3): a kind=ai lead decomposes a boss request into a
// workflow graph (nodes = employees, edges = dependencies) plus initial
// dispatch tasks, all in one POST /api/decompose call.
func TestDecompose(t *testing.T) {
	e := newEnv(t)

	a, err := e.st.CreateDepartment("A", nil)
	if err != nil {
		t.Fatal(err)
	}
	l1, l1tok := makeEmployee(t, e, "lead", store.KindAI, a)
	if err := e.st.SetDepartmentLeader(a, l1); err != nil {
		t.Fatal(err)
	}
	b, err := e.st.CreateDepartment("B", &a)
	if err != nil {
		t.Fatal(err)
	}
	w1, w1tok := makeEmployee(t, e, "w1", store.KindAI, b)
	w2, _ := makeEmployee(t, e, "w2", store.KindAI, b)
	// unrelated top-level employee for the route-403 case
	c, _ := e.st.CreateDepartment("C", nil)
	c1, _ := makeEmployee(t, e, "c1", store.KindAI, c)

	// 1) Happy path: lead decomposes onto its two subordinates with one
	// dependency edge and one initial task.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/decompose", l1tok, map[string]any{
		"name":  "release-flow",
		"nodes": []any{map[string]any{"employee_id": w1, "x": 1, "y": 2}, map[string]any{"employee_id": w2}},
		"edges": []any{map[string]any{"from": w1, "to": w2}},
		"tasks": []any{map[string]any{"employee_id": w1, "input": "draft the release notes"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("decompose: status %d body=%v", code, body)
	}
	wfID := int(body["workflow_id"].(float64))
	if wfID == 0 {
		t.Fatalf("decompose response missing workflow_id: %v", body)
	}
	if body["name"] != "release-flow" {
		t.Errorf("name = %v", body["name"])
	}
	rawNodes, _ := body["nodes"].([]any)
	if len(rawNodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(rawNodes))
	}
	rawEdges, _ := body["edges"].([]any)
	if len(rawEdges) != 1 {
		t.Fatalf("edges = %d, want 1", len(rawEdges))
	}
	if got := rawEdges[0].(map[string]any)["from_employee_id"]; int(got.(float64)) != w1 {
		t.Errorf("edge from_employee_id = %v, want %d", got, w1)
	}
	rawTasks, _ := body["tasks"].([]any)
	if len(rawTasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(rawTasks))
	}
	if got := rawTasks[0].(map[string]any)["status"]; got != store.TaskQueued {
		t.Errorf("initial task status = %v, want %s", got, store.TaskQueued)
	}

	// 2) The workflow is persisted and visible via the existing GET endpoint.
	code, body = req(t, http.MethodGet, fmt.Sprintf("%s/api/workflows/%d", e.srv.URL, wfID), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("get workflow: status %d body=%v", code, body)
	}
	gotNodes, _ := body["nodes"].([]any)
	if len(gotNodes) != 2 {
		t.Errorf("persisted nodes = %d, want 2", len(gotNodes))
	}
	gotEdges, _ := body["edges"].([]any)
	if len(gotEdges) != 1 {
		t.Errorf("persisted edges = %d, want 1", len(gotEdges))
	}

	// 3) The lead's own decompose-request task (boss dispatch) is queryable.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "拆解发布流程"})
	if code != http.StatusCreated {
		t.Fatalf("boss dispatch to lead: %d %v", code, body)
	}
	leadTask := int(body["task_id"].(float64))
	code, body = req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks/%d", e.srv.URL, leadTask), l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("get lead task: status %d body=%v", code, body)
	}
	if body["status"] != store.TaskQueued {
		t.Errorf("lead task status = %v, want queued", body["status"])
	}

	// 4) The initial task landed in w1's queue, requested by the lead, linked
	// to the new workflow (workflow link verified at the store layer: the
	// task row API does not expose workflow_id).
	tasks := tasksOf(t, e, w1tok, w1)
	if len(tasks) != 1 {
		t.Fatalf("w1 tasks = %d, want 1", len(tasks))
	}
	if tasks[0]["status"] != store.TaskQueued {
		t.Errorf("w1 task status = %v, want queued", tasks[0]["status"])
	}
	if int(tasks[0]["requested_by"].(float64)) != l1 {
		t.Errorf("w1 task requested_by = %v, want %d", tasks[0]["requested_by"], l1)
	}
	initTask, err := e.st.GetTask(int(rawTasks[0].(map[string]any)["id"].(float64)))
	if err != nil {
		t.Fatalf("get initial task: %v", err)
	}
	if initTask.WorkflowID == nil || *initTask.WorkflowID != wfID {
		t.Errorf("initial task workflow_id = %v, want %d", initTask.WorkflowID, wfID)
	}

	// 5) Audit recorded the decompose with the acting employee.
	entries, err := e.st.AuditList(100)
	if err != nil {
		t.Fatal(err)
	}
	var found *store.AuditEntry
	for i := range entries {
		if entries[i].Action == "decompose.create" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no decompose.create audit entry")
	}
	if found.ActorKind != "employee" || found.ActorID != l1 {
		t.Errorf("audit actor = %s/%d, want employee/%d", found.ActorKind, found.ActorID, l1)
	}

	// 6) Authz: admin tokens cannot decompose.
	bad := map[string]any{
		"name":  "x",
		"nodes": []any{map[string]any{"employee_id": w1}, map[string]any{"employee_id": w2}},
	}
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/decompose", adminTok, bad); code != http.StatusForbidden {
		t.Errorf("admin decompose: got %d, want 403", code)
	}

	// 7) Non-AI employee is rejected.
	_, h1tok := makeEmployee(t, e, "h1", store.KindHuman, a)
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/decompose", h1tok, bad); code != http.StatusForbidden {
		t.Errorf("human decompose: got %d, want 403", code)
	}

	// 8) Route 403: node outside the caller's dispatch chain.
	bodyOut := map[string]any{
		"name":  "out",
		"nodes": []any{map[string]any{"employee_id": w1}, map[string]any{"employee_id": c1}},
	}
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/decompose", l1tok, bodyOut); code != http.StatusForbidden {
		t.Errorf("out-of-chain node: got %d, want 403", code)
	}
	// w1 (worker, no down-chain) cannot decompose at all.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/decompose", w1tok, bad); code != http.StatusForbidden {
		t.Errorf("worker decompose: got %d, want 403", code)
	}

	// 9) Validation 400s (table-driven).
	cases := []struct {
		name string
		body map[string]any
	}{
		{"empty name", map[string]any{"name": "  ", "nodes": bad["nodes"]}},
		{"single node", map[string]any{"name": "one", "nodes": []any{map[string]any{"employee_id": w1}}}},
		{"duplicate node", map[string]any{"name": "dup", "nodes": []any{map[string]any{"employee_id": w1}, map[string]any{"employee_id": w1}}}},
		{"edge non-node", map[string]any{"name": "e", "nodes": bad["nodes"], "edges": []any{map[string]any{"from": w1, "to": c1}}}},
		{"self-loop", map[string]any{"name": "e", "nodes": bad["nodes"], "edges": []any{map[string]any{"from": w1, "to": w1}}}},
		{"task non-node", map[string]any{"name": "e", "nodes": bad["nodes"], "tasks": []any{map[string]any{"employee_id": c1, "input": "x"}}}},
		{"task empty input", map[string]any{"name": "e", "nodes": bad["nodes"], "tasks": []any{map[string]any{"employee_id": w1, "input": "  "}}}},
	}
	for _, tc := range cases {
		if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/decompose", l1tok, tc.body); code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", tc.name, code)
		}
	}
}
