package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"whatbark/internal/store"
)

// TestGroupTasksAPI (S24/S25/S29): GET /api/groups/{id}/tasks — the group task
// board. Boss + any member may read; non-member 403; missing group 404;
// limit must be 1..500. Rows are exactly {id,status,input,employee_id,
// requested_by,employee_name,requested_by_name,created_at,message_id,
// project_id} (S29: project_id always present, null when unbound; project_name
// only when the task is bound to a project that still exists; no
// output/kind/priority), id descending, message_id always present (null
// when no dispatch anchor), requested_by always present (0 = boss).
func TestGroupTasksAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	// x1: employee outside the group (non-member).
	deptC, err := e.st.CreateDepartment("C", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, x1tok := makeEmployee(t, e, "X1", store.KindAI, deptC)

	// Group A: boss-created, members {0, l1, w1}.
	gidA := makeGroup(t, e, adminTok, "s24-a", []int{l1, w1})

	// Boss dispatches to two different members inside the group.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gidA, "to": w1, "kind": "dispatch", "body": "job one"})
	if code != http.StatusCreated {
		t.Fatalf("boss dispatch 1: got %d (body %v)", code, body)
	}
	msg1 := int(body["message_id"].(float64))
	task1 := int(body["task_id"].(float64))

	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gidA, "to": l1, "kind": "dispatch", "body": "job two"})
	if code != http.StatusCreated {
		t.Fatalf("boss dispatch 2: got %d (body %v)", code, body)
	}
	task2 := int(body["task_id"].(float64))

	board := func(tok string, qs string) (int, []map[string]any) {
		t.Helper()
		code, body := req(t, http.MethodGet,
			fmt.Sprintf("%s/api/groups/%d/tasks%s", e.srv.URL, gidA, qs), tok, nil)
		if code != http.StatusOK {
			return code, nil
		}
		raw, _ := body["tasks"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, m := range raw {
			out = append(out, m.(map[string]any))
		}
		return code, out
	}
	rowOf := func(rows []map[string]any, taskID int) (map[string]any, bool) {
		t.Helper()
		for _, row := range rows {
			if row["id"] == float64(taskID) {
				return row, true
			}
		}
		return nil, false
	}

	// 1) Boss view: 200, two rows id-desc, exact key set, correct names.
	code, rows := board(adminTok, "")
	if code != http.StatusOK {
		t.Fatalf("boss board: got %d", code)
	}
	if len(rows) != 2 {
		t.Fatalf("board rows = %d, want 2: %v", len(rows), rows)
	}
	gotIDs := make([]any, 0, 2)
	for _, row := range rows {
		gotIDs = append(gotIDs, row["id"])
	}
	if !slices.Equal(gotIDs, []any{float64(task2), float64(task1)}) {
		t.Fatalf("ids = %v, want [%d %d] (id desc)", gotIDs, task2, task1)
	}
	wantKeys := []string{"id", "status", "input", "employee_id", "requested_by",
		"employee_name", "requested_by_name", "created_at", "message_id", "project_id"}
	for i, row := range rows {
		if len(row) != len(wantKeys) {
			t.Fatalf("row %d keys = %v, want exactly %v", i, row, wantKeys)
		}
		for _, k := range wantKeys {
			if _, ok := row[k]; !ok {
				t.Fatalf("row %d missing key %q: %v", i, k, row)
			}
		}
		if row["status"] != store.TaskQueued {
			t.Errorf("row %d status = %v, want queued", i, row["status"])
		}
	}
	r1, _ := rowOf(rows, task1)
	if r1["employee_name"] != "W1" || r1["requested_by_name"] != "boss" {
		t.Errorf("task1 names = %v/%v, want W1/boss", r1["employee_name"], r1["requested_by_name"])
	}
	if r1["input"] != "job one" || r1["employee_id"] != float64(w1) {
		t.Errorf("task1 row = %v", r1)
	}
	if r1["message_id"] != float64(msg1) {
		t.Errorf("task1 message_id = %v, want %d", r1["message_id"], msg1)
	}
	// S25: boss-dispatched rows carry requested_by = 0.
	if r1["requested_by"] != float64(0) {
		t.Errorf("task1 requested_by = %v, want 0 (boss dispatch)", r1["requested_by"])
	}
	r2, _ := rowOf(rows, task2)
	if r2["employee_name"] != "L1" || r2["requested_by_name"] != "boss" {
		t.Errorf("task2 names = %v/%v, want L1/boss", r2["employee_name"], r2["requested_by_name"])
	}
	if r2["requested_by"] != float64(0) {
		t.Errorf("task2 requested_by = %v, want 0 (boss dispatch)", r2["requested_by"])
	}

	// 2) Visibility policy: member who owns only task1 still sees both rows.
	if code, rows := board(w1tok, ""); code != http.StatusOK || len(rows) != 2 {
		t.Fatalf("member w1 board: code=%d rows=%d, want 200/2 (member sees group's tasks)", code, len(rows))
	}

	// 3) Non-member: 403.
	if code, _ := board(x1tok, ""); code != http.StatusForbidden {
		t.Fatalf("non-member x1 board: got %d, want 403", code)
	}

	// 4) Missing group 404; malformed group id 400.
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/groups/99999/tasks", adminTok, nil); code != http.StatusNotFound {
		t.Fatalf("missing group board: got %d, want 404", code)
	}
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/groups/abc/tasks", adminTok, nil); code != http.StatusBadRequest {
		t.Fatalf("malformed group id board: got %d, want 400", code)
	}

	// 5) Limit validation: 1 keeps newest, 500 is the ceiling, bad values 400.
	if code, rows := board(adminTok, "?limit=1"); code != http.StatusOK || len(rows) != 1 || rows[0]["id"] != float64(task2) {
		t.Fatalf("limit=1: code=%d rows=%v, want 1 row (newest task %d)", code, rows, task2)
	}
	if code, rows := board(adminTok, "?limit=500"); code != http.StatusOK || len(rows) != 2 {
		t.Fatalf("limit=500: code=%d rows=%d, want 200/2", code, len(rows))
	}
	for _, bad := range []string{"?limit=0", "?limit=abc", "?limit=501", "?limit=-1"} {
		if code, _ := req(t, http.MethodGet,
			fmt.Sprintf("%s/api/groups/%d/tasks%s", e.srv.URL, gidA, bad), adminTok, nil); code != http.StatusBadRequest {
			t.Errorf("limit %s: got %d, want 400", bad, code)
		}
	}

	// 6) Status tracking: start task1 -> running; finish -> done; the row
	//    still carries no output key.
	if code, _ := req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, task1), w1tok, nil); code != http.StatusOK {
		t.Fatalf("start task1: got %d", code)
	}
	if _, rows := board(adminTok, ""); true {
		r, ok := rowOf(rows, task1)
		if !ok || r["status"] != store.TaskRunning {
			t.Fatalf("task1 after start = %v, want running", r)
		}
	}
	if code, _ := req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, task1), w1tok,
		map[string]any{"output": "shipped"}); code != http.StatusOK {
		t.Fatalf("finish task1: got %d", code)
	}
	if _, rows := board(adminTok, ""); true {
		r, ok := rowOf(rows, task1)
		if !ok || r["status"] != store.TaskDone {
			t.Fatalf("task1 after finish = %v, want done", r)
		}
		if _, hasOutput := r["output"]; hasOutput {
			t.Errorf("board row carries output key: %v", r)
		}
	}

	// 7) A group task without a dispatch message (created directly in the
	//    store): message_id must still be present and null.
	gidARef := gidA
	ghost, err := e.st.CreateTask(l1, store.KindDispatch, "ghost job", nil, nil, 0, 0, &gidARef)
	if err != nil {
		t.Fatalf("CreateTask ghost: %v", err)
	}
	if _, rows := board(adminTok, ""); true {
		r, ok := rowOf(rows, ghost)
		if !ok {
			t.Fatalf("ghost task %d missing from board", ghost)
		}
		if v, has := r["message_id"]; !has || v != nil {
			t.Errorf("ghost row message_id = %v, want null (key present)", v)
		}
		if r["requested_by_name"] != "boss" {
			t.Errorf("ghost requested_by_name = %v, want boss", r["requested_by_name"])
		}
	}

	// 8) Boss exemption: employee-created group (boss is NOT a member) —
	//    the boss (admin token, EmpID==0) may still read the board.
	gidB := makeGroup(t, e, l1tok, "s24-b", []int{w1})
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gidB, "to": w1, "kind": "dispatch", "body": "exempt job"})
	if code != http.StatusCreated {
		t.Fatalf("l1 dispatch in own group: got %d (body %v)", code, body)
	}
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/groups/%d/tasks", e.srv.URL, gidB), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("boss board on non-member group: got %d, want 200 (boss exemption)", code)
	}
	raw := body["tasks"].([]any)
	if len(raw) != 1 {
		t.Fatalf("boss board rows on gidB = %d, want 1", len(raw))
	}
	// S25: requested_by must be present on every row (here l1, the requester).
	bossRow := raw[0].(map[string]any)
	if v, has := bossRow["requested_by"]; !has || v != float64(l1) {
		t.Errorf("boss-exempt row requested_by = %v (present=%v), want %d (l1)", v, has, l1)
	}
}
