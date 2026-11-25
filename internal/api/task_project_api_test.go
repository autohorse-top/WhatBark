package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// makeProject creates a project through the admin API and returns its id.
func makeProject(t *testing.T, e *env, name string) int {
	t.Helper()
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/projects", adminTok,
		map[string]any{"name": name})
	if code != http.StatusCreated {
		t.Fatalf("POST /api/projects %q: got %d, want 201 (body %v)", name, code, body)
	}
	return int(body["id"].(float64))
}

// TestGroupDispatchProject (S29): a group dispatch may bind the created task
// to an existing project (201 + task.ProjectID set, visible via
// GET /api/tasks?project=N); an unknown project id -> 400 (flow error, no
// task created); kind=chat with project_id -> 400; group rows still reject
// task_id (S14/S22 regression).
func TestGroupDispatchProject(t *testing.T) {
	e := newEnv(t)
	l1, _, w1, _ := topo(t, e)
	pA := makeProject(t, e, "Alpha")
	gid := makeGroup(t, e, adminTok, "s29-a", []int{l1, w1})

	// a) boss group dispatch with a valid project: 201, task bound to it.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gid, "to": w1, "kind": "dispatch", "body": "proj job", "project_id": pA})
	if code != http.StatusCreated {
		t.Fatalf("group dispatch with project: got %d, want 201 (body %v)", code, body)
	}
	taskID := int(body["task_id"].(float64))
	task, err := e.st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask(%d): %v", taskID, err)
	}
	if task.ProjectID == nil || *task.ProjectID != pA {
		t.Fatalf("task.ProjectID = %v, want %d", task.ProjectID, pA)
	}
	if task.GroupID == nil || *task.GroupID != gid {
		t.Fatalf("task.GroupID = %v, want %d", task.GroupID, gid)
	}

	// Reverse lookup through GET /api/tasks?project=N.
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/tasks?project=%d", e.srv.URL, pA), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/tasks?project=%d: got %d", pA, code)
	}
	raw, _ := body["tasks"].([]any)
	var found bool
	for _, m := range raw {
		row := m.(map[string]any)
		if row["id"] == float64(taskID) {
			found = true
			if row["project_id"] != float64(pA) {
				t.Errorf("task row project_id = %v, want %d", row["project_id"], pA)
			}
		}
	}
	if !found {
		t.Errorf("task %d missing from GET /api/tasks?project=%d", taskID, pA)
	}

	// b) unknown project: 400 with the flow error; nothing was created.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gid, "to": w1, "kind": "dispatch", "body": "x", "project_id": 99999})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown project: got %d, want 400 (body %v)", code, body)
	}
	if body["error"] != "flow: unknown project 99999" {
		t.Errorf("error = %v, want %q", body["error"], "flow: unknown project 99999")
	}
	tasks, err := e.st.ListGroupTasks(gid, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("group tasks after rejected dispatch = %d, want 1 (no orphan task)", len(tasks))
	}

	// c) kind=chat with project_id: 400.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gid, "kind": "chat", "body": "x", "project_id": pA})
	if code != http.StatusBadRequest {
		t.Fatalf("chat with project_id: got %d, want 400 (body %v)", code, body)
	}

	// d) group row with task_id: 400 (regression).
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gid, "to": w1, "kind": "dispatch", "body": "x", "task_id": 1})
	if code != http.StatusBadRequest {
		t.Fatalf("group row with task_id: got %d, want 400", code)
	}
}

// TestGroupBoardProjectFilter (S29): GET /api/groups/{id}/tasks?project=P —
// P>0 returns only that project's tasks, P=0 only project-less tasks,
// non-integer/negative values 400, absent parameter returns everything.
// Rows always carry project_id (null when unbound); project_name only when
// the task has a project that still exists.
func TestGroupBoardProjectFilter(t *testing.T) {
	e := newEnv(t)
	l1, _, w1, _ := topo(t, e)
	pA := makeProject(t, e, "Alpha")
	pB := makeProject(t, e, "Beta")
	gid := makeGroup(t, e, adminTok, "s29-b", []int{l1, w1})

	dispatch := func(proj *int, body string) int {
		t.Helper()
		reqBody := map[string]any{"group": gid, "to": w1, "kind": "dispatch", "body": body}
		if proj != nil {
			reqBody["project_id"] = *proj
		}
		code, b := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok, reqBody)
		if code != http.StatusCreated {
			t.Fatalf("dispatch %q: got %d, want 201 (body %v)", body, code, b)
		}
		return int(b["task_id"].(float64))
	}
	a1 := dispatch(&pA, "alpha one")
	a2 := dispatch(&pA, "alpha two")
	b1 := dispatch(&pB, "beta one")
	none := dispatch(nil, "loose")

	board := func(qs string) (int, []map[string]any) {
		t.Helper()
		code, body := req(t, http.MethodGet,
			fmt.Sprintf("%s/api/groups/%d/tasks%s", e.srv.URL, gid, qs), adminTok, nil)
		if code != http.StatusOK && code != http.StatusBadRequest {
			t.Fatalf("board %s: unexpected status %d", qs, code)
		}
		raw, _ := body["tasks"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, m := range raw {
			out = append(out, m.(map[string]any))
		}
		return code, out
	}
	idsOf := func(rows []map[string]any) []int {
		out := make([]int, 0, len(rows))
		for _, r := range rows {
			out = append(out, int(r["id"].(float64)))
		}
		return out
	}

	// e-1) no parameter: all four tasks, id descending.
	code, rows := board("")
	if code != http.StatusOK {
		t.Fatalf("board no filter: got %d", code)
	}
	if got := idsOf(rows); !slices.Equal(got, []int{none, b1, a2, a1}) {
		t.Fatalf("all rows = %v, want [%d %d %d %d]", got, none, b1, a2, a1)
	}

	// e-2) ?project=pA: only the two alpha tasks, with name.
	code, rows = board(fmt.Sprintf("?project=%d", pA))
	if code != http.StatusOK {
		t.Fatalf("board project pA: got %d", code)
	}
	if got := idsOf(rows); !slices.Equal(got, []int{a2, a1}) {
		t.Fatalf("pA rows = %v, want [%d %d]", got, a2, a1)
	}
	if rows[0]["project_id"] != float64(pA) {
		t.Errorf("pA row project_id = %v, want %d", rows[0]["project_id"], pA)
	}
	if rows[0]["project_name"] != "Alpha" {
		t.Errorf("pA row project_name = %v, want Alpha", rows[0]["project_name"])
	}

	// e-3) ?project=pB: only the beta task.
	code, rows = board(fmt.Sprintf("?project=%d", pB))
	if code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("board project pB: code=%d rows=%v, want 200/1", code, rows)
	}
	if rows[0]["project_id"] != float64(pB) || rows[0]["project_name"] != "Beta" {
		t.Errorf("pB row = %v, want project %d named Beta", rows[0], pB)
	}

	// e-4) ?project=0: only the project-less task; project_id present-null,
	// no project_name key.
	code, rows = board("?project=0")
	if code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("board project=0: code=%d rows=%v, want 200/1", code, rows)
	}
	row := rows[0]
	if row["id"] != float64(none) {
		t.Errorf("project=0 row id = %v, want %d", row["id"], none)
	}
	v, has := row["project_id"]
	if !has || v != nil {
		t.Errorf("project-less row project_id = %v (present=%v), want null with key present", v, has)
	}
	if _, has := row["project_name"]; has {
		t.Errorf("project-less row carries project_name: %v", row)
	}

	// e-5) invalid project values: 400.
	for _, qs := range []string{"?project=abc", "?project=-1"} {
		code, _ := board(qs)
		if code != http.StatusBadRequest {
			t.Errorf("board %s: got %d, want 400", qs, code)
		}
	}
}
