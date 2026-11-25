package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"whatbark/internal/store"
)

// TestSearchAPI (S19-C): GET /api/search — q/limit validation, contact
// reachability filtering (routing graph), self exclusion, and message
// scoping to the caller's own conversations (1:1 + own groups; boss sees all
// 1:1s and group messages of member groups only, S32/M1), plus S32 .groups
// name rows (member-scoped, name/kind/member_count).
func TestSearchAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	// Department C is a separate root: its leader X1 is unreachable from w1
	// (no report chain, not a leader, no workflow edges), but reachable from
	// l1 (leader-to-leader chat) and from the boss (always).
	c, err := e.st.CreateDepartment("C", nil)
	if err != nil {
		t.Fatal(err)
	}
	x1, _ := makeEmployee(t, e, "X1", store.KindAI, c)
	if err := e.st.SetDepartmentLeader(c, x1); err != nil {
		t.Fatal(err)
	}

	// --- validation ---
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/search?q=a", w1tok, nil); code != http.StatusBadRequest {
		t.Fatalf("q too short: got %d want 400", code)
	}
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/search?q=hello&limit=0", w1tok, nil); code != http.StatusBadRequest {
		t.Fatalf("limit=0: got %d want 400", code)
	}
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/search?q=hello&limit=abc", w1tok, nil); code != http.StatusBadRequest {
		t.Fatalf("limit=abc: got %d want 400", code)
	}

	search := func(tok, q, limit string) (int, map[string]any) {
		t.Helper()
		u := fmt.Sprintf("%s/api/search?q=%s", e.srv.URL, q)
		if limit != "" {
			u += "&limit=" + limit
		}
		return req(t, http.MethodGet, u, tok, nil)
	}
	contactIDs := func(body map[string]any) []int {
		out := []int{}
		for _, c := range body["contacts"].([]any) {
			out = append(out, int(c.(map[string]any)["employee_id"].(float64)))
		}
		return out
	}
	msgIDs := func(body map[string]any) []int {
		out := []int{}
		for _, m := range body["messages"].([]any) {
			out = append(out, int(m.(map[string]any)["id"].(float64)))
		}
		return out
	}

	// --- contact filtering ---
	// w1 can reach l1 (report up the chain).
	code, body := search(w1tok, "L1", "")
	if code != http.StatusOK {
		t.Fatalf("w1 search L1: %d %v", code, body)
	}
	if !slices.Contains(contactIDs(body), l1) {
		t.Fatalf("w1 search L1: contacts=%v want %d", contactIDs(body), l1)
	}
	// Self is excluded.
	code, body = search(w1tok, "W1", "")
	if code != http.StatusOK {
		t.Fatalf("w1 search W1: %d", code)
	}
	if slices.Contains(contactIDs(body), w1) {
		t.Fatal("self listed in own contacts")
	}
	// x1 is unreachable from w1, but reachable from l1 (leader chat) and boss.
	code, body = search(w1tok, "X1", "")
	if code != http.StatusOK {
		t.Fatalf("w1 search X1: %d", code)
	}
	if slices.Contains(contactIDs(body), x1) {
		t.Fatal("x1 listed for w1 despite being unreachable")
	}
	code, body = search(l1tok, "X1", "")
	if code != http.StatusOK {
		t.Fatalf("l1 search X1: %d", code)
	}
	if !slices.Contains(contactIDs(body), x1) {
		t.Fatal("x1 not listed for l1 (leader-to-leader chat should allow)")
	}
	code, body = search(adminTok, "X1", "")
	if code != http.StatusOK {
		t.Fatalf("boss search X1: %d", code)
	}
	if !slices.Contains(contactIDs(body), x1) {
		t.Fatal("x1 not listed for boss")
	}

	// --- message scoping ---
	// Boss dispatches to l1: l1 finds it, w1 does not.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "s19c alpha marker"})
	if code != http.StatusCreated {
		t.Fatalf("boss dispatch: %d %v", code, body)
	}
	alphaID := int(body["message_id"].(float64))

	code, body = search(l1tok, "alpha", "")
	if code != http.StatusOK {
		t.Fatalf("l1 search alpha: %d", code)
	}
	if !slices.Contains(msgIDs(body), alphaID) {
		t.Fatal("l1 cannot find own dispatch")
	}
	code, body = search(w1tok, "alpha", "")
	if code != http.StatusOK {
		t.Fatalf("w1 search alpha: %d", code)
	}
	if slices.Contains(msgIDs(body), alphaID) {
		t.Fatal("w1 found a message outside its conversations")
	}

	// Group message: visible to members, rows carry group_id.
	gid := makeGroup(t, e, l1tok, "sg", []int{w1})
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", w1tok,
		map[string]any{"group": gid, "kind": "chat", "body": "s19c beta marker"})
	if code != http.StatusCreated {
		t.Fatalf("group chat: %d %v", code, body)
	}
	betaID := int(body["message_id"].(float64))

	code, body = search(w1tok, "beta", "")
	if code != http.StatusOK {
		t.Fatalf("w1 search beta: %d", code)
	}
	raw := body["messages"].([]any)
	if len(raw) != 1 {
		t.Fatalf("w1 search beta: %d rows, want 1", len(raw))
	}
	row := raw[0].(map[string]any)
	if int(row["id"].(float64)) != betaID {
		t.Fatalf("row id = %v, want %d", row["id"], betaID)
	}
	if int(row["group_id"].(float64)) != gid {
		t.Fatalf("row group_id = %v, want %d", row["group_id"], gid)
	}

	// S32/M2: .groups rows are member-scoped and carry name/kind/member_count.
	code, body = search(w1tok, "sg", "")
	if code != http.StatusOK {
		t.Fatalf("w1 search sg: %d %v", code, body)
	}
	grows := body["groups"].([]any)
	var sgRow map[string]any
	for _, g := range grows {
		if int(g.(map[string]any)["group_id"].(float64)) == gid {
			sgRow = g.(map[string]any)
		}
	}
	if sgRow == nil {
		t.Fatalf("w1 search sg: groups=%v, want gid %d", grows, gid)
	}
	if sgRow["name"] != "sg" || sgRow["kind"] != "group" || int(sgRow["member_count"].(float64)) != 2 {
		t.Fatalf("w1 group row = %v, want name=sg kind=group member_count=2", sgRow)
	}
	// M1: boss is not a member of gid -> no group row, and the group message
	// must not leak into the boss's message search either (thread would 403).
	code, body = search(adminTok, "sg", "")
	if code != http.StatusOK {
		t.Fatalf("boss search sg: %d %v", code, body)
	}
	for _, g := range body["groups"].([]any) {
		if int(g.(map[string]any)["group_id"].(float64)) == gid {
			t.Fatalf("boss saw group row of a group he is not a member of: %v", g)
		}
	}
	code, body = search(adminTok, "beta", "")
	if code != http.StatusOK {
		t.Fatalf("boss search s19c beta: %d %v", code, body)
	}
	if slices.Contains(msgIDs(body), betaID) {
		t.Fatalf("M1: boss found group message %d from non-member group %d", betaID, gid)
	}

	// Boss sees all 1:1 messages (group messages only for member groups, M1).
	code, body = search(adminTok, "alpha", "")
	if code != http.StatusOK {
		t.Fatalf("boss search alpha: %d", code)
	}
	if !slices.Contains(msgIDs(body), alphaID) {
		t.Fatal("boss cannot find a message")
	}

	// limit is honored.
	code, body = search(l1tok, "alpha", "1")
	if code != http.StatusOK {
		t.Fatalf("limit search: %d", code)
	}
	if got := len(body["messages"].([]any)); got != 1 {
		t.Fatalf("limit=1 returned %d rows, want 1", got)
	}
}
