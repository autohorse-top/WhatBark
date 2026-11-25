package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"whatbark/internal/store"
)

// makeGroup creates a group through the API and returns its id.
func makeGroup(t *testing.T, e *env, tok, name string, members []int) int {
	t.Helper()
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/groups", tok,
		map[string]any{"name": name, "member_ids": members})
	if code != http.StatusCreated {
		t.Fatalf("POST /api/groups: got %d, want 201 (body %v)", code, body)
	}
	return int(body["id"].(float64))
}

// TestCreateGroupAPI: POST /api/groups — 201 on success (name trimmed,
// creator auto-joined), 400 on validation failure.
func TestCreateGroupAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, _ := topo(t, e)

	// Valid: employee creator, name trimmed, creator auto-joined,
	// default kind = "group" (S21 backward compatibility).
	gid := makeGroup(t, e, l1tok, "  squad  ", []int{w1})
	g, err := e.st.GetGroup(gid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if g.Name != "squad" || g.CreatedBy != l1 || g.Kind != "group" {
		t.Fatalf("group = %+v, want name squad creator %d kind group", g, l1)
	}
	members, err := e.st.GroupMemberIDs(gid)
	if err != nil {
		t.Fatalf("GroupMemberIDs: %v", err)
	}
	if !slices.Equal(members, []int{l1, w1}) {
		t.Fatalf("members = %v, want [%d %d]", members, l1, w1)
	}

	// Validation failures -> 400.
	bad := []map[string]any{
		{"name": "   ", "member_ids": []int{w1}},                      // blank name
		{"name": "solo", "member_ids": []int{l1}},                     // only the creator
		{"name": "ghost", "member_ids": []int{9999}},                  // unknown member
		{"name": "badkind", "member_ids": []int{w1}, "kind": "bogus"}, // S21: invalid kind
	}
	for i, b := range bad {
		code, body := req(t, http.MethodPost, e.srv.URL+"/api/groups", l1tok, b)
		if code != http.StatusBadRequest {
			t.Errorf("case %d: got %d, want 400 (body %v)", i, code, body)
		}
	}

	// Boss can create groups too (boss auto-joins as member 0).
	gid2 := makeGroup(t, e, adminTok, "boss-squad", []int{w1})
	members, _ = e.st.GroupMemberIDs(gid2)
	if !slices.Equal(members, []int{0, w1}) {
		t.Fatalf("boss group members = %v, want [0 %d]", members, w1)
	}
}

// TestGroupKindAPI: S21 — kind=channel is boss-only (employee 403), kind
// echoes in POST 201 / GET /api/groups / /api/conversations, invalid kind
// is 400, and plain groups keep kind=group.
func TestGroupKindAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	// Boss creates a channel: 201 with kind=channel echoed.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/groups", adminTok,
		map[string]any{"name": "ops-chan", "member_ids": []int{l1, w1}, "kind": "channel"})
	if code != http.StatusCreated {
		t.Fatalf("boss channel: got %d, want 201 (body %v)", code, body)
	}
	if body["kind"] != "channel" {
		t.Errorf("boss channel 201 kind = %v, want channel", body["kind"])
	}
	chanID := int(body["id"].(float64))
	if g, err := e.st.GetGroup(chanID); err != nil || g.Kind != "channel" {
		t.Fatalf("stored channel = %+v (err %v), want kind=channel", g, err)
	}

	// Employee POST kind=channel -> 403 (boss-only surface).
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/groups", l1tok,
		map[string]any{"name": "nope-chan", "member_ids": []int{w1}, "kind": "channel"})
	if code != http.StatusForbidden {
		t.Fatalf("employee channel: got %d, want 403 (body %v)", code, body)
	}
	if gcs, err := e.st.ListGroupConversations(0); err != nil || len(gcs) != 1 {
		t.Errorf("boss group conversations after rejected channel = %+v (err %v), want 1", gcs, err)
	}

	// Invalid kind -> 400 (both boss and employee).
	for _, tok := range []string{adminTok, l1tok} {
		code, body = req(t, http.MethodPost, e.srv.URL+"/api/groups", tok,
			map[string]any{"name": "bogus-chan", "member_ids": []int{w1}, "kind": "bogus"})
		if code != http.StatusBadRequest {
			t.Errorf("kind=bogus: got %d, want 400 (body %v)", code, body)
		}
	}

	// Employee's plain group still works and keeps kind=group.
	plain := makeGroup(t, e, l1tok, "plain-squad", []int{w1})
	if g, err := e.st.GetGroup(plain); err != nil || g.Kind != "group" {
		t.Fatalf("plain group = %+v (err %v), want kind=group", g, err)
	}

	// GET /api/groups/{id} echoes kind.
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, chanID), w1tok, nil)
	if code != http.StatusOK || body["kind"] != "channel" {
		t.Errorf("GET channel: got %d kind=%v, want 200 channel", code, body["kind"])
	}
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, plain), w1tok, nil)
	if code != http.StatusOK || body["kind"] != "group" {
		t.Errorf("GET plain: got %d kind=%v, want 200 group", code, body["kind"])
	}

	// /api/conversations: group rows carry is_group=true + the real kind.
	cons := conversations(t, e, w1tok)
	if gc := findConv(t, cons, -(chanID + 1)); gc["is_group"] != true || gc["kind"] != "channel" {
		t.Errorf("channel conversation row = %v, want is_group=true kind=channel", gc)
	}
	if gc := findConv(t, cons, -(plain + 1)); gc["is_group"] != true || gc["kind"] != "group" {
		t.Errorf("plain conversation row = %v, want is_group=true kind=group", gc)
	}
}

// TestGroupMessageSend: POST /api/messages with group — member 201,
// non-member 403, bad payload shapes 400.
func TestGroupMessageSend(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, _ := topo(t, e)
	gid := makeGroup(t, e, adminTok, "g", []int{l1, w1}) // {0, l1, w1}

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gid, "kind": "chat", "body": "hi squad"})
	if code != http.StatusCreated {
		t.Fatalf("member send: got %d, want 201 (body %v)", code, body)
	}
	if _, ok := body["message_id"]; !ok {
		t.Fatalf("member send: message_id missing in %v", body)
	}
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gid, "kind": "chat", "body": "boss here"})
	if code != http.StatusCreated {
		t.Fatalf("boss send: got %d, want 201", code)
	}

	// Payload-shape violations -> 400.
	bad := []map[string]any{
		{"group": gid, "to": w1, "kind": "chat", "body": "x"}, // to must be absent
		{"group": gid, "kind": "dispatch", "body": "x"},       // S22: dispatch now needs "to"
		{"group": gid, "kind": "chat", "body": "x", "project_id": 1},
		{"group": gid, "kind": "chat", "body": ""}, // empty body, no attachments
	}
	for i, b := range bad {
		code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok, b)
		if code != http.StatusBadRequest {
			t.Errorf("case %d: got %d, want 400 (body %v)", i, code, body)
		}
	}

	// Non-member -> 403.
	dept, err := e.st.CreateDepartment("dept-x", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, x1tok := makeEmployee(t, e, "X1", store.KindAI, dept)
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", x1tok,
		map[string]any{"group": gid, "kind": "chat", "body": "intruder"})
	if code != http.StatusForbidden {
		t.Fatalf("non-member send: got %d, want 403 (body %v)", code, body)
	}
}

// TestGroupThreadList: GET /api/messages?group=G — member 200 with the
// group thread (boss rows have from_id null), non-member 403, selector
// violations 400.
func TestGroupThreadList(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)
	dept, err := e.st.CreateDepartment("dept-x", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, x1tok := makeEmployee(t, e, "X1", store.KindAI, dept)

	gid := makeGroup(t, e, adminTok, "g", []int{l1, w1})
	req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gid, "kind": "chat", "body": "first"})
	req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gid, "kind": "chat", "body": "from boss"})

	code, body := req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gid), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("group thread: got %d, want 200", code)
	}
	msgs, _ := body["messages"].([]any)
	// S17: group creation inserts a system message, so 3 total (system + 2 chat).
	if len(msgs) != 3 {
		t.Fatalf("group thread = %d msgs, want 3: %v", len(msgs), body)
	}
	first, second, third := msgs[0].(map[string]any), msgs[1].(map[string]any), msgs[2].(map[string]any)
	if first["kind"] != "system" || first["body"] != "boss created a group" || first["from_id"] != nil {
		t.Errorf("first row = %v, want system creation event", first)
	}
	if second["body"] != "first" || second["from_id"] != float64(l1) || second["kind"] != "chat" {
		t.Errorf("second row = %v, want l1 chat 'first'", second)
	}
	if third["body"] != "from boss" || third["from_id"] != nil {
		t.Errorf("third row = %v, want boss chat 'from boss' with null from_id", third)
	}

	// Non-member -> 403; unknown group -> 403; selector violations -> 400.
	if code, _ = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gid), x1tok, nil); code != http.StatusForbidden {
		t.Errorf("non-member list: got %d, want 403", code)
	}
	if code, _ = req(t, http.MethodGet, e.srv.URL+"/api/messages?group=9999", l1tok, nil); code != http.StatusForbidden {
		t.Errorf("unknown group: got %d, want 403", code)
	}
	for _, qs := range []string{
		fmt.Sprintf("group=%d&with=0", gid),
		fmt.Sprintf("group=%d&to=1", gid),
		"group=abc",
		"group=-1",
	} {
		if code, _ = req(t, http.MethodGet,
			fmt.Sprintf("%s/api/messages?%s", e.srv.URL, qs), l1tok, nil); code != http.StatusBadRequest {
			t.Errorf("?%s: got %d, want 400", qs, code)
		}
	}
}

// TestGroupReadCursorAPI: POST /api/messages/read {group} — member 200 and
// the conversation unread clears; non-member 403; with/group mutually
// exclusive 400.
func TestGroupReadCursorAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)
	dept, err := e.st.CreateDepartment("dept-x", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, x1tok := makeEmployee(t, e, "X1", store.KindAI, dept)

	gid := makeGroup(t, e, adminTok, "g", []int{l1, w1})
	req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gid, "kind": "chat", "body": "one"})
	req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"group": gid, "kind": "chat", "body": "two"})

	// w1 sees the group in its conversation list, unread = 2.
	cons := conversations(t, e, w1tok)
	gc, ok := convOf(t, cons, -(gid + 1))
	if !ok {
		t.Fatalf("group %d missing from w1 conversations: %v", gid, cons)
	}
	if gc["is_group"] != true || gc["kind"] != "group" || gc["group_id"] != float64(gid) {
		t.Errorf("group row meta = %v", gc)
	}
	if gc["name"] != "g" || gc["member_count"] != float64(3) {
		t.Errorf("group row name/member_count = %v/%v", gc["name"], gc["member_count"])
	}
	if gc["last_msg_kind"] != "chat" || gc["last_msg_body"] != "two" {
		t.Errorf("group row last msg = %v/%v", gc["last_msg_kind"], gc["last_msg_body"])
	}
	if got := convInt(gc, "unread"); got != 2 {
		t.Fatalf("group unread = %d, want 2", got)
	}

	// Mark read -> unread clears.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages/read", w1tok,
		map[string]any{"group": gid})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("group read: got %d %v, want 200 ok", code, body)
	}
	if gc, _ := convOf(t, conversations(t, e, w1tok), -(gid + 1)); convInt(gc, "unread") != 0 {
		t.Errorf("group unread after read != 0: %v", gc)
	}

	// Non-member cannot mark the group read.
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages/read", x1tok,
		map[string]any{"group": gid})
	if code != http.StatusForbidden {
		t.Errorf("non-member read: got %d, want 403", code)
	}

	// with/group are mutually exclusive.
	for _, b := range []map[string]any{
		{"with": 0, "group": gid},
		{},
	} {
		code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages/read", w1tok, b)
		if code != http.StatusBadRequest {
			t.Errorf("read %v: got %d, want 400", b, code)
		}
	}
}

// TestConversationsGroupAnd1to1: group and 1:1 entries coexist in the
// conversation list, mixed by latest message id, and group traffic never
// pollutes the 1:1 thread (and vice versa).
func TestConversationsGroupAnd1to1(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	gid := makeGroup(t, e, adminTok, "squad", []int{l1, w1})
	code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gid, "kind": "chat", "body": "gmsg"})
	if code != http.StatusCreated {
		t.Fatalf("group msg: got %d, want 201", code)
	}
	// Newer 1:1 traffic: l1 dispatches to w1 (creates a task).
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "job"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: got %d, want 201", code)
	}

	cons := conversations(t, e, w1tok)
	if len(cons) != 2 {
		t.Fatalf("w1 conversations = %d entries, want 2: %v", len(cons), cons)
	}
	// The 1:1 entry is newer and must sort first.
	if cons[0]["peer_id"] != float64(l1) || cons[0]["is_group"] != false {
		t.Fatalf("first entry = %v, want 1:1 with l1", cons[0])
	}
	if cons[0]["last_msg_kind"] != "dispatch" {
		t.Errorf("1:1 last_msg_kind = %v, want dispatch", cons[0]["last_msg_kind"])
	}
	gc := cons[1]
	if gc["peer_id"] != float64(-(gid+1)) || gc["is_group"] != true {
		t.Fatalf("second entry = %v, want group %d", gc, gid)
	}
	if gc["last_msg_kind"] != "chat" || gc["last_msg_body"] != "gmsg" {
		t.Errorf("group last msg = %v/%v", gc["last_msg_kind"], gc["last_msg_body"])
	}

	// The 1:1 thread contains only the dispatch, never the group message.
	code, body := req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?with=%d", e.srv.URL, l1), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("1:1 thread: got %d, want 200", code)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["kind"] != "dispatch" {
		t.Fatalf("1:1 thread = %v, want only the dispatch", msgs)
	}
	// S17: the group thread contains the creation system message + the chat.
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gid), w1tok, nil)
	msgs, _ = body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("group thread = %v, want 2 msgs", msgs)
	}
	if msgs[0].(map[string]any)["kind"] != "system" || msgs[0].(map[string]any)["body"] != "boss created a group" {
		t.Errorf("group thread[0] = %v, want system creation event", msgs[0])
	}
	if msgs[1].(map[string]any)["body"] != "gmsg" || msgs[1].(map[string]any)["kind"] != "chat" {
		t.Errorf("group thread[1] = %v, want l1 chat 'gmsg'", msgs[1])
	}
}

// TestGetGroupAPI: GET /api/groups/{id} — member vs manager view,
// candidates only for manager, non-member 403, unknown group 404, bad id 400.
func TestGetGroupAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, _ := topo(t, e)

	// Boss creates a group with l1 and w1.
	gid := makeGroup(t, e, adminTok, "settings", []int{l1, w1})

	// Member (l1): can_manage=false, no candidates, muted/ignored false.
	code, body := req(t, http.MethodGet,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("member GET group: got %d, want 200 (body %v)", code, body)
	}
	if body["can_manage"] != false {
		t.Errorf("member can_manage = %v, want false", body["can_manage"])
	}
	if _, ok := body["candidates"]; ok {
		t.Error("member view should not include candidates")
	}
	if body["muted"] != false || body["ignored"] != false {
		t.Errorf("member prefs = muted:%v ignored:%v, want false/false", body["muted"], body["ignored"])
	}
	// creator_name should be "boss"
	if body["creator_name"] != "boss" {
		t.Errorf("creator_name = %v, want 'boss'", body["creator_name"])
	}

	// Creator (boss/admin): can_manage=true, candidates present.
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("creator GET group: got %d, want 200", code)
	}
	if body["can_manage"] != true {
		t.Errorf("creator can_manage = %v, want true", body["can_manage"])
	}
	cands, ok := body["candidates"].([]any)
	if !ok {
		t.Fatal("creator view missing candidates")
	}
	// Boss (0) is already a member, should NOT be in candidates.
	for _, c := range cands {
		cm := c.(map[string]any)
		if cm["employee_id"] == float64(0) {
			t.Error("boss should not appear in candidates (already a member)")
		}
	}

	// Non-member: 403
	dept, _ := e.st.CreateDepartment("dept-nm", nil)
	_, x1tok := makeEmployee(t, e, "X9", store.KindAI, dept)
	code, _ = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), x1tok, nil)
	if code != http.StatusForbidden {
		t.Errorf("non-member GET group: got %d, want 403", code)
	}

	// Unknown group: 404
	code, _ = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/groups/99999", e.srv.URL), l1tok, nil)
	if code != http.StatusNotFound {
		t.Errorf("unknown group: got %d, want 404", code)
	}

	// Bad id: 400
	code, _ = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/groups/abc", e.srv.URL), l1tok, nil)
	if code != http.StatusBadRequest {
		t.Errorf("bad id: got %d, want 400", code)
	}
}

// TestAddGroupMembersAPI: POST /api/groups/{id}/members — creator and boss
// can add; non-manager member and non-member get 403; empty/duplicate 400;
// unknown group 404.
func TestAddGroupMembersAPI(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)

	// l1 creates a group with w1.
	gid := makeGroup(t, e, l1tok, "am-test", []int{w1})

	// Create a new employee to add.
	dept, _ := e.st.CreateDepartment("dept-am", nil)
	x1, _ := makeEmployee(t, e, "X9", store.KindAI, dept)

	// Creator (l1) adds x1: 200.
	code, body := req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/members", e.srv.URL, gid), l1tok,
		map[string]any{"employee_ids": []int{x1}})
	if code != http.StatusOK {
		t.Fatalf("creator add member: got %d, want 200 (body %v)", code, body)
	}
	added, _ := body["added"].([]any)
	if len(added) != 1 || added[0].(float64) != float64(x1) {
		t.Errorf("added = %v, want [%d]", added, x1)
	}

	// Boss (admin) adds another member: 200.
	x2, _ := makeEmployee(t, e, "X10", store.KindAI, dept)
	code, body = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/members", e.srv.URL, gid), adminTok,
		map[string]any{"employee_ids": []int{x2}})
	if code != http.StatusOK {
		t.Fatalf("boss add member: got %d, want 200 (body %v)", code, body)
	}

	// Non-manager member (w1) tries to add: 403.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/members", e.srv.URL, gid), w1tok,
		map[string]any{"employee_ids": []int{x1}})
	if code != http.StatusForbidden {
		t.Errorf("non-manager add: got %d, want 403", code)
	}

	// Non-member tries to add: 403.
	dept2, _ := e.st.CreateDepartment("dept-nm2", nil)
	_, x3tok := makeEmployee(t, e, "X11", store.KindAI, dept2)
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/members", e.srv.URL, gid), x3tok,
		map[string]any{"employee_ids": []int{x1}})
	if code != http.StatusForbidden {
		t.Errorf("non-member add: got %d, want 403", code)
	}

	// Empty member list: 400.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/members", e.srv.URL, gid), l1tok,
		map[string]any{"employee_ids": []int{}})
	if code != http.StatusBadRequest {
		t.Errorf("empty member list: got %d, want 400", code)
	}

	// All duplicates (w1 already a member): 400.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/members", e.srv.URL, gid), l1tok,
		map[string]any{"employee_ids": []int{w1}})
	if code != http.StatusBadRequest {
		t.Errorf("duplicate member: got %d, want 400", code)
	}

	// Unknown group: 404.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/99999/members", e.srv.URL), l1tok,
		map[string]any{"employee_ids": []int{x1}})
	if code != http.StatusNotFound {
		t.Errorf("unknown group add: got %d, want 404", code)
	}
}

// TestGroupPrefsAPI: POST /api/groups/{id}/prefs — set/clear muted and
// ignored independently, empty body 400, non-member 403, unknown group 404,
// and /api/conversations reflects the flags.
func TestGroupPrefsAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	gid := makeGroup(t, e, adminTok, "prefs-test", []int{l1, w1})

	// l1 sets muted=true only.
	code, body := req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/prefs", e.srv.URL, gid), l1tok,
		map[string]any{"muted": true})
	if code != http.StatusOK {
		t.Fatalf("set muted: got %d, want 200 (body %v)", code, body)
	}
	if body["muted"] != true || body["ignored"] != false {
		t.Errorf("prefs = muted:%v ignored:%v, want true/false", body["muted"], body["ignored"])
	}

	// l1 sets ignored=true (muted should persist).
	code, body = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/prefs", e.srv.URL, gid), l1tok,
		map[string]any{"ignored": true})
	if code != http.StatusOK {
		t.Fatalf("set ignored: got %d, want 200", code)
	}
	if body["muted"] != true || body["ignored"] != true {
		t.Errorf("prefs after ignored set = muted:%v ignored:%v, want true/true", body["muted"], body["ignored"])
	}

	// l1 clears both.
	code, body = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/prefs", e.srv.URL, gid), l1tok,
		map[string]any{"muted": false, "ignored": false})
	if code != http.StatusOK {
		t.Fatalf("clear prefs: got %d, want 200", code)
	}
	if body["muted"] != false || body["ignored"] != false {
		t.Errorf("prefs after clear = muted:%v ignored:%v, want false/false", body["muted"], body["ignored"])
	}

	// Empty body: 400.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/prefs", e.srv.URL, gid), l1tok,
		map[string]any{})
	if code != http.StatusBadRequest {
		t.Errorf("empty prefs body: got %d, want 400", code)
	}

	// w1 sets muted independently (l1's prefs unchanged).
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/prefs", e.srv.URL, gid), w1tok,
		map[string]any{"muted": true})
	if code != http.StatusOK {
		t.Errorf("w1 set muted: got %d, want 200", code)
	}

	// Verify /api/conversations shows w1's group as muted.
	cons := conversations(t, e, w1tok)
	gc := findConv(t, cons, -(gid + 1))
	if gc["muted"] != true {
		t.Errorf("w1 group muted in conversations = %v, want true", gc["muted"])
	}
	if gc["ignored"] != false {
		t.Errorf("w1 group ignored in conversations = %v, want false", gc["ignored"])
	}

	// Non-member: 403.
	dept, _ := e.st.CreateDepartment("dept-pf", nil)
	_, x1tok := makeEmployee(t, e, "XP", store.KindAI, dept)
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/prefs", e.srv.URL, gid), x1tok,
		map[string]any{"muted": true})
	if code != http.StatusForbidden {
		t.Errorf("non-member prefs: got %d, want 403", code)
	}

	// Unknown group: 404.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/99999/prefs", e.srv.URL), l1tok,
		map[string]any{"muted": true})
	if code != http.StatusNotFound {
		t.Errorf("unknown group prefs: got %d, want 404", code)
	}
}

// TestConversationsGroupPin: after creating a group, it should appear in the
// conversation list with last_msg_body = "{creator} created a group" and rank first
// (newest) when no 1:1 traffic exists.
func TestConversationsGroupPin(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, _ := topo(t, e)

	// l1 creates a group with w1 (creator = L1).
	gid := makeGroup(t, e, l1tok, "pin-test", []int{w1})

	// w1 should see the group with the system message as last msg.
	cons := conversations(t, e, "tok-W1")
	if len(cons) == 0 {
		t.Fatal("w1 conversations empty")
	}
	gc := findConv(t, cons, -(gid + 1))
	if gc["last_msg_body"] != "L1 created a group" {
		t.Errorf("last_msg_body = %v, want 'L1 created a group'", gc["last_msg_body"])
	}
	if gc["last_msg_kind"] != "system" {
		t.Errorf("last_msg_kind = %v, want system", gc["last_msg_kind"])
	}
	// Group entries carry default prefs (false/false) before any update.
	if gc["muted"] != false || gc["ignored"] != false {
		t.Errorf("group entry default prefs = muted:%v ignored:%v, want false/false", gc["muted"], gc["ignored"])
	}
	// No 1:1 traffic, so the group should be the only entry (hence first).
	if len(cons) != 1 {
		t.Fatalf("w1 conversations = %d entries, want 1 (only the group)", len(cons))
	}
}

// TestRenameGroupAPI (S26): PATCH /api/groups/{id} — creator and boss can
// rename, non-manager member / non-member 403, bad names 400, unknown group
// 404, bad id 400.
func TestRenameGroupAPI(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)

	gid := makeGroup(t, e, l1tok, "rename-me", []int{w1}) // creator l1, {l1, w1}

	// Creator renames: 200, name trimmed and persisted.
	code, body := req(t, http.MethodPatch,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), l1tok,
		map[string]any{"name": "  renamed  "})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("creator rename: got %d %v, want 200 ok", code, body)
	}
	if g, err := e.st.GetGroup(gid); err != nil || g.Name != "renamed" {
		t.Fatalf("group name = %q (err %v), want renamed", g.Name, err)
	}

	// Boss may rename even without being a member.
	code, _ = req(t, http.MethodPatch,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), adminTok,
		map[string]any{"name": "boss-renamed"})
	if code != http.StatusOK {
		t.Fatalf("boss rename: got %d, want 200", code)
	}
	if g, _ := e.st.GetGroup(gid); g.Name != "boss-renamed" {
		t.Fatalf("group name = %q, want boss-renamed", g.Name)
	}

	// Non-manager member (w1) -> 403.
	code, _ = req(t, http.MethodPatch,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), w1tok,
		map[string]any{"name": "nope"})
	if code != http.StatusForbidden {
		t.Errorf("non-manager rename: got %d, want 403", code)
	}

	// Non-member employee -> 403.
	dept, _ := e.st.CreateDepartment("dept-rn", nil)
	_, x1tok := makeEmployee(t, e, "XR", store.KindAI, dept)
	code, _ = req(t, http.MethodPatch,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), x1tok,
		map[string]any{"name": "nope"})
	if code != http.StatusForbidden {
		t.Errorf("non-member rename: got %d, want 403", code)
	}

	// Invalid names -> 400.
	for _, name := range []string{"", "   ", strings.Repeat("群", 65)} {
		code, _ = req(t, http.MethodPatch,
			fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), l1tok,
			map[string]any{"name": name})
		if code != http.StatusBadRequest {
			t.Errorf("rename %q: got %d, want 400", name, code)
		}
	}
	// Missing name field -> 400.
	code, _ = req(t, http.MethodPatch,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), l1tok,
		map[string]any{})
	if code != http.StatusBadRequest {
		t.Errorf("rename without name: got %d, want 400", code)
	}

	// Unknown group -> 404; malformed id -> 400.
	code, _ = req(t, http.MethodPatch,
		fmt.Sprintf("%s/api/groups/99999", e.srv.URL), l1tok,
		map[string]any{"name": "x"})
	if code != http.StatusNotFound {
		t.Errorf("rename unknown group: got %d, want 404", code)
	}
	code, _ = req(t, http.MethodPatch,
		fmt.Sprintf("%s/api/groups/abc", e.srv.URL), l1tok,
		map[string]any{"name": "x"})
	if code != http.StatusBadRequest {
		t.Errorf("rename bad id: got %d, want 400", code)
	}
}

// TestRemoveGroupMemberAPI (S26): DELETE /api/groups/{id}/members/{eid} —
// self-leave 200, manager removal 200, boss row 403, non-manager removing
// another 403, non-member 403, unknown member 404, <2 members left 400.
func TestRemoveGroupMemberAPI(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)
	gid := makeGroup(t, e, l1tok, "rm-api", []int{w1}) // {l1, w1}

	// Third member so removals can keep the >=2 invariant.
	dept, _ := e.st.CreateDepartment("dept-rm", nil)
	x1, x1tok := makeEmployee(t, e, "XR", store.KindAI, dept)
	code, _ := req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/members", e.srv.URL, gid), l1tok,
		map[string]any{"employee_ids": []int{x1}})
	if code != http.StatusOK {
		t.Fatalf("add x1: got %d, want 200", code)
	}

	rmURL := func(eid int) string {
		return fmt.Sprintf("%s/api/groups/%d/members/%d", e.srv.URL, gid, eid)
	}

	// Boss row is protected: 403.
	code, _ = req(t, http.MethodDelete, rmURL(0), l1tok, nil)
	if code != http.StatusForbidden {
		t.Errorf("remove boss: got %d, want 403", code)
	}

	// Unknown member: 404.
	code, _ = req(t, http.MethodDelete, rmURL(999), l1tok, nil)
	if code != http.StatusNotFound {
		t.Errorf("remove unknown member: got %d, want 404", code)
	}

	// Non-manager member removing another member: 403.
	code, _ = req(t, http.MethodDelete, rmURL(x1), w1tok, nil)
	if code != http.StatusForbidden {
		t.Errorf("non-manager remove: got %d, want 403", code)
	}

	// Self-leave: 200.
	code, _ = req(t, http.MethodDelete, rmURL(x1), x1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("self leave: got %d, want 200", code)
	}
	if ok, _ := e.st.IsGroupMember(gid, x1); ok {
		t.Error("x1 still a member after self-leave")
	}

	// <2 members: boss removes w1 from the now 2-member {l1, w1} -> 400.
	code, _ = req(t, http.MethodDelete, rmURL(w1), adminTok, nil)
	if code != http.StatusBadRequest {
		t.Errorf("remove leaving <2 members: got %d, want 400", code)
	}

	// Manager removal: re-add x1, then creator removes w1 -> 200.
	code, _ = req(t, http.MethodPost,
		fmt.Sprintf("%s/api/groups/%d/members", e.srv.URL, gid), l1tok,
		map[string]any{"employee_ids": []int{x1}})
	if code != http.StatusOK {
		t.Fatalf("re-add x1: got %d, want 200", code)
	}
	code, _ = req(t, http.MethodDelete, rmURL(w1), l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("creator remove: got %d, want 200", code)
	}
	if ok, _ := e.st.IsGroupMember(gid, w1); ok {
		t.Error("w1 still a member after removal")
	}
	if ok, _ := e.st.IsGroupMember(gid, x1); !ok {
		t.Error("x1 lost membership unexpectedly")
	}

	// Non-member removing a member: 403 (caller-scope check first).
	dept2, _ := e.st.CreateDepartment("dept-rm2", nil)
	_, x2tok := makeEmployee(t, e, "XQ", store.KindAI, dept2)
	code, _ = req(t, http.MethodDelete, rmURL(x1), x2tok, nil)
	if code != http.StatusForbidden {
		t.Errorf("non-member remove: got %d, want 403", code)
	}
}

// TestDeleteGroupAPI (S26): DELETE /api/groups/{id} — non-manager 403, open
// tasks 409 with the count, successful delete cascades members/messages and
// clears conv_reads while the group's tasks survive with group_id NULL;
// unknown group 404; boss may delete any group.
func TestDeleteGroupAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)
	gid := makeGroup(t, e, l1tok, "del-api", []int{w1}) // creator l1, {l1, w1}

	// Non-manager member -> 403.
	code, _ := req(t, http.MethodDelete,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), w1tok, nil)
	if code != http.StatusForbidden {
		t.Errorf("non-manager delete: got %d, want 403", code)
	}

	// An open (queued) group task blocks deletion: 409 with the count.
	gidRef := gid
	tid, err := e.st.CreateTask(w1, store.KindDispatch, "open job", nil, nil, l1, 0, &gidRef)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	code, body := req(t, http.MethodDelete,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), l1tok, nil)
	if code != http.StatusConflict {
		t.Fatalf("delete with open task: got %d, want 409 (body %v)", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "1") {
		t.Errorf("409 error = %q, want it to mention 1 open task", msg)
	}

	// Cancel the task, then delete: 200.
	if err := e.st.SetTaskStatus(tid, store.TaskCanceled, ""); err != nil {
		t.Fatalf("SetTaskStatus: %v", err)
	}
	code, body = req(t, http.MethodDelete,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), l1tok, nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("delete: got %d %v, want 200 ok", code, body)
	}

	// The group is gone: row, membership, messages, and read cursors.
	if _, err := e.st.GetGroup(gid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetGroup after delete = %v, want ErrNotFound", err)
	}
	if ok, _ := e.st.IsGroupMember(gid, l1); ok {
		t.Error("l1 still a member after delete")
	}
	if msgs, _ := e.st.GroupMessages(gid, 0); len(msgs) != 0 {
		t.Errorf("group messages after delete = %d, want 0", len(msgs))
	}
	var reads int
	if err := e.st.DB().QueryRow(
		"SELECT COUNT(*) FROM conv_reads WHERE peer_id = ?", -(gid + 1)).Scan(&reads); err != nil {
		t.Fatalf("count conv_reads: %v", err)
	}
	if reads != 0 {
		t.Errorf("conv_reads after delete = %d, want 0", reads)
	}
	// The task survives with group_id NULL (ON DELETE SET NULL).
	tk, err := e.st.GetTask(tid)
	if err != nil {
		t.Fatalf("GetTask after delete: %v", err)
	}
	if tk.GroupID != nil {
		t.Errorf("task.group_id = %v after delete, want nil", *tk.GroupID)
	}

	// Unknown group -> 404.
	code, _ = req(t, http.MethodDelete,
		fmt.Sprintf("%s/api/groups/99999", e.srv.URL), l1tok, nil)
	if code != http.StatusNotFound {
		t.Errorf("delete unknown group: got %d, want 404", code)
	}

	// Boss may delete any group even without membership.
	gid2 := makeGroup(t, e, l1tok, "del-boss", []int{w1})
	code, _ = req(t, http.MethodDelete,
		fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid2), adminTok, nil)
	if code != http.StatusOK {
		t.Errorf("boss delete: got %d, want 200", code)
	}
}
