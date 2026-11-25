package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"whatbark/internal/store"
)

// TestDepartmentChannelCreate (S31): POST /api/departments returns 201
// {id, group_id}; the channel is kind=channel, created_by=0, name = the
// trimmed department name, boss is the sole member, and carries exactly one
// system message. GET /api/departments exposes group_id. Validation is 400;
// non-admin is 403.
func TestDepartmentChannelCreate(t *testing.T) {
	e := newEnv(t)

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/departments", adminTok, map[string]any{"name": "  研发部 "})
	if code != http.StatusCreated {
		t.Fatalf("create dept: status %d body %v", code, body)
	}
	deptID, ok := intFromAny(t, body, "id")
	if !ok {
		t.Fatalf("create dept body = %v, want id", body)
	}
	gid, ok := intFromAny(t, body, "group_id")
	if !ok {
		t.Fatalf("create dept body = %v, want group_id", body)
	}
	if deptID <= 0 || gid <= 0 {
		t.Fatalf("ids must be positive: %+v", body)
	}

	d, err := e.st.GetDepartment(deptID)
	if err != nil || d.GroupID == nil || *d.GroupID != gid || d.Name != "研发部" {
		t.Fatalf("dept = %+v (err %v), want name=研发部 group_id=%d", d, err, gid)
	}
	g, err := e.st.GetGroup(gid)
	if err != nil || g.Kind != "channel" || g.CreatedBy != 0 || g.Name != "研发部" {
		t.Fatalf("group = %+v (err %v), want kind=channel created_by=0 name=研发部", g, err)
	}
	members, err := e.st.GroupMemberIDs(gid)
	if err != nil || len(members) != 1 || members[0] != 0 {
		t.Fatalf("members = %v (err %v), want [0]", members, err)
	}
	msgs, err := e.st.GroupMessages(gid, 0)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("group messages = %+v (err %v), want exactly 1", msgs, err)
	}
	if msgs[0].Kind != store.MsgSystem || msgs[0].FromID != nil ||
		msgs[0].Body != `boss created a department channel "研发部"` {
		t.Fatalf("system message = %+v, want boss-created department channel event", msgs[0])
	}

	// GET /api/departments carries group_id.
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/departments", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("list depts: status %d", code)
	}
	rows, _ := body["departments"].([]any)
	if len(rows) != 1 {
		t.Fatalf("departments = %v, want 1 row", rows)
	}
	row := rows[0].(map[string]any)
	if int(row["id"].(float64)) != deptID || int(row["group_id"].(float64)) != gid {
		t.Fatalf("list row = %v, want id=%d group_id=%d", row, deptID, gid)
	}

	// Duplicate at the same level -> 400; 65 runes -> 400.
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/departments", adminTok, map[string]any{"name": "研发部"})
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate top-level name: status %d, want 400", code)
	}
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/departments", adminTok,
		map[string]any{"name": strings.Repeat("部", 65)})
	if code != http.StatusBadRequest {
		t.Fatalf("65-rune name: status %d, want 400", code)
	}
	// Non-admin cannot create departments (adminOnly).
	empID, tok := makeEmployee(t, e, "dev-a", "ai", deptID)
	_ = empID
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/departments", tok, map[string]any{"name": "X部"})
	if code != http.StatusForbidden {
		t.Fatalf("employee creates dept: status %d, want 403", code)
	}
}

// TestDepartmentRenameAPI (S31): PUT /api/departments/{id} renames the
// department and syncs the channel name (200 {ok, id, name, group_id});
// validation / same-level duplicate -> 400; unknown dept -> 404; non-admin
// -> 403.
func TestDepartmentRenameAPI(t *testing.T) {
	e := newEnv(t)
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/departments", adminTok, map[string]any{"name": "甲部"})
	if code != http.StatusCreated {
		t.Fatalf("seed dept: status %d", code)
	}
	deptID, _ := intFromAny(t, body, "id")
	gid, _ := intFromAny(t, body, "group_id")
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/departments", adminTok, map[string]any{"name": "乙部"})
	if code != http.StatusCreated {
		t.Fatalf("seed sibling: status %d", code)
	}

	// Success: rename + channel name sync.
	code, body = req(t, http.MethodPut, fmt.Sprintf("%s/api/departments/%d", e.srv.URL, deptID),
		adminTok, map[string]any{"name": "丙部"})
	if code != http.StatusOK {
		t.Fatalf("rename: status %d body %v", code, body)
	}
	if body["ok"] != true || body["name"] != "丙部" || int(body["group_id"].(float64)) != gid {
		t.Fatalf("rename body = %v, want ok/name=丙部/group_id=%d", body, gid)
	}
	g, err := e.st.GetGroup(gid)
	if err != nil || g.Name != "丙部" {
		t.Fatalf("channel name = %+v (err %v), want 丙部 (synced)", g, err)
	}
	d, err := e.st.GetDepartment(deptID)
	if err != nil || d.Name != "丙部" {
		t.Fatalf("department name = %+v (err %v), want 丙部", d, err)
	}

	// Validation failures -> 400 (department unchanged).
	for _, name := range []string{"", "   ", strings.Repeat("部", 65), "乙部"} {
		code, _ = req(t, http.MethodPut, fmt.Sprintf("%s/api/departments/%d", e.srv.URL, deptID),
			adminTok, map[string]any{"name": name})
		if code != http.StatusBadRequest {
			t.Fatalf("rename %q: status %d, want 400", name, code)
		}
	}
	if got, _ := e.st.GetDepartment(deptID); got.Name != "丙部" {
		t.Fatalf("department renamed by invalid request: %q", got.Name)
	}
	if got, _ := e.st.GetGroup(gid); got.Name != "丙部" {
		t.Fatalf("channel renamed by invalid request: %q", got.Name)
	}

	// Unknown department -> 404; non-admin -> 403.
	empID, tok := makeEmployee(t, e, "dev-b", "ai", deptID)
	_ = empID
	code, _ = req(t, http.MethodPut, e.srv.URL+"/api/departments/999", adminTok, map[string]any{"name": "X"})
	if code != http.StatusNotFound {
		t.Fatalf("rename unknown dept: status %d, want 404", code)
	}
	code, _ = req(t, http.MethodPut, fmt.Sprintf("%s/api/departments/%d", e.srv.URL, deptID),
		tok, map[string]any{"name": "X部"})
	if code != http.StatusForbidden {
		t.Fatalf("employee renames dept: status %d, want 403", code)
	}
}

// TestDepartmentDeleteAPI (S31): DELETE /api/departments/{id} blocks with 400
// while the department has employees, 409 (open-task wording) while the
// channel has open tasks, then 200 after the task is canceled — the
// department row and the channel row are both gone, the task survives with
// group_id NULL. Unknown dept -> 404. Legacy departments (no channel)
// rename/delete without touching any group.
func TestDepartmentDeleteAPI(t *testing.T) {
	e := newEnv(t)
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/departments", adminTok, map[string]any{"name": "目标部"})
	if code != http.StatusCreated {
		t.Fatalf("seed dept: status %d", code)
	}
	deptID, _ := intFromAny(t, body, "id")
	gid, _ := intFromAny(t, body, "group_id")

	// Employees in the department -> 400.
	empID, _ := makeEmployee(t, e, "emp-in", "ai", deptID)
	code, _ = req(t, http.MethodDelete, fmt.Sprintf("%s/api/departments/%d", e.srv.URL, deptID), adminTok, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("delete with employee: status %d, want 400", code)
	}
	if err := e.st.DeleteEmployee(empID); err != nil {
		t.Fatalf("cleanup employee: %v", err)
	}

	// Open task anchored to the channel -> 409 with the S26 wording.
	otherDept, err := e.st.CreateDepartment("其他部", nil)
	if err != nil {
		t.Fatalf("seed other dept: %v", err)
	}
	worker, _ := makeEmployee(t, e, "worker-x", "ai", otherDept)
	gidRef := gid
	tid, err := e.st.CreateTask(worker, store.KindDispatch, "open job", nil, nil, 0, 0, &gidRef)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	code, body = req(t, http.MethodDelete, fmt.Sprintf("%s/api/departments/%d", e.srv.URL, deptID), adminTok, nil)
	if code != http.StatusConflict {
		t.Fatalf("delete with open task: status %d body %v, want 409", code, body)
	}
	if body["error"] != "Group still has 1 unfinished task(s)" {
		t.Fatalf("409 body = %v, want Group still has 1 unfinished task(s)", body)
	}
	if _, err := e.st.GetDepartment(deptID); err != nil {
		t.Fatalf("department gone after refused delete: %v", err)
	}
	if _, err := e.st.GetGroup(gid); err != nil {
		t.Fatalf("channel gone after refused delete: %v", err)
	}

	// Cancel the task, then the delete succeeds.
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/cancel", e.srv.URL, tid), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("cancel task: status %d, want 200", code)
	}
	code, body = req(t, http.MethodDelete, fmt.Sprintf("%s/api/departments/%d", e.srv.URL, deptID), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("delete after cancel: status %d body %v", code, body)
	}
	if body["ok"] != true {
		t.Fatalf("delete body = %v, want ok:true", body)
	}
	if _, err := e.st.GetDepartment(deptID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("department after delete = %v, want ErrNotFound", err)
	}
	if _, err := e.st.GetGroup(gid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("channel after delete = %v, want ErrNotFound", err)
	}
	task, err := e.st.GetTask(tid)
	if err != nil || task.GroupID != nil || task.Status != store.TaskCanceled {
		t.Fatalf("task after delete = %+v (err %v), want canceled with group_id NULL", task, err)
	}

	// Unknown department -> 404.
	code, _ = req(t, http.MethodDelete, e.srv.URL+"/api/departments/999", adminTok, nil)
	if code != http.StatusNotFound {
		t.Fatalf("delete unknown dept: status %d, want 404", code)
	}

	// Legacy department (no channel): rename + delete are plain 200s and
	// never touch the groups table.
	var nGroups int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM groups`).Scan(&nGroups); err != nil {
		t.Fatalf("count groups: %v", err)
	}
	legacy, err := e.st.CreateDepartment("存量部", nil)
	if err != nil {
		t.Fatalf("legacy dept: %v", err)
	}
	// GET /api/departments shows group_id null for the legacy row.
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/departments", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("list depts: status %d", code)
	}
	rows, _ := body["departments"].([]any)
	var foundLegacy bool
	for _, r := range rows {
		row := r.(map[string]any)
		if int(row["id"].(float64)) == legacy {
			foundLegacy = true
			if row["group_id"] != nil {
				t.Fatalf("legacy row group_id = %v, want null", row["group_id"])
			}
		}
	}
	if !foundLegacy {
		t.Fatalf("legacy dept %d missing from list %v", legacy, rows)
	}
	code, _ = req(t, http.MethodPut, fmt.Sprintf("%s/api/departments/%d", e.srv.URL, legacy),
		adminTok, map[string]any{"name": "存量改名"})
	if code != http.StatusOK {
		t.Fatalf("legacy rename: status %d, want 200", code)
	}
	code, _ = req(t, http.MethodDelete, fmt.Sprintf("%s/api/departments/%d", e.srv.URL, legacy), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("legacy delete: status %d, want 200", code)
	}
	var after int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM groups`).Scan(&after); err != nil {
		t.Fatalf("recount groups: %v", err)
	}
	if after != nGroups {
		t.Fatalf("groups changed around legacy dept ops: %d -> %d", nGroups, after)
	}
}

// TestEmployeeAutoJoinChannel (S31): POST /api/employees into a department
// that owns a channel auto-joins the new employee (broadcast group.members),
// who can then post into the channel; a non-member employee gets 403 on the
// same channel; a legacy department (no channel) creates the employee with no
// side effect on the groups table.
func TestEmployeeAutoJoinChannel(t *testing.T) {
	e := newEnv(t)
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/departments", adminTok, map[string]any{"name": "入队部"})
	if code != http.StatusCreated {
		t.Fatalf("seed dept: status %d", code)
	}
	deptID, _ := intFromAny(t, body, "id")
	gid, _ := intFromAny(t, body, "group_id")

	code, body = req(t, http.MethodPost, e.srv.URL+"/api/employees", adminTok, map[string]any{
		"name": "new-eng", "role": "dev", "kind": "ai", "department_id": deptID,
	})
	if code != http.StatusCreated {
		t.Fatalf("create employee: status %d body %v", code, body)
	}
	empID, _ := intFromAny(t, body, "id")
	empTok, _ := body["token"].(string)
	if empTok == "" {
		t.Fatalf("create employee body = %v, want token", body)
	}

	members, err := e.st.GroupMemberIDs(gid)
	if err != nil || len(members) != 2 {
		t.Fatalf("channel members = %v (err %v), want [0, %d]", members, err, empID)
	}
	if members[0] != 0 || members[1] != empID {
		t.Fatalf("channel members = %v, want [0, %d]", members, empID)
	}
	// The auto-joined employee can post into the channel.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", empTok, map[string]any{
		"group": gid, "kind": "chat", "body": "hello channel",
	})
	if code != http.StatusCreated {
		t.Fatalf("member posts to channel: status %d body %v", code, body)
	}

	// A non-member employee in another department gets 403 on that channel.
	otherDept, err := e.st.CreateDepartment("他部", nil)
	if err != nil {
		t.Fatalf("seed other dept: %v", err)
	}
	_, outsiderTok := makeEmployee(t, e, "outsider", "ai", otherDept)
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", outsiderTok, map[string]any{
		"group": gid, "kind": "chat", "body": "not a member",
	})
	if code != http.StatusForbidden {
		t.Fatalf("non-member posts to channel: status %d, want 403", code)
	}

	// Legacy department (no channel): employee creation has no group side
	// effect.
	var before int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM groups`).Scan(&before); err != nil {
		t.Fatalf("count groups: %v", err)
	}
	legacy, err := e.st.CreateDepartment("无频道部", nil)
	if err != nil {
		t.Fatalf("legacy dept: %v", err)
	}
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/employees", adminTok, map[string]any{
		"name": "legacy-eng", "role": "ops", "kind": "ai", "department_id": legacy,
	})
	if code != http.StatusCreated {
		t.Fatalf("create legacy-dept employee: status %d body %v", code, body)
	}
	var after int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM groups`).Scan(&after); err != nil {
		t.Fatalf("recount groups: %v", err)
	}
	if after != before {
		t.Fatalf("groups changed by legacy-dept employee: %d -> %d", before, after)
	}
}

// intFromAny extracts a positive integer field from a decoded JSON body.
func intFromAny(t *testing.T, body map[string]any, key string) (int, bool) {
	t.Helper()
	f, ok := body[key].(float64)
	if !ok || f <= 0 {
		return 0, false
	}
	return int(f), true
}
