package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"whatbark/internal/store"
)

// conversations fetches the caller's conversation list and returns the rows.
func conversations(t *testing.T, e *env, tok string) []map[string]any {
	t.Helper()
	code, body := req(t, http.MethodGet, e.srv.URL+"/api/conversations", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/conversations: status %d", code)
	}
	raw, _ := body["conversations"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

func findConv(t *testing.T, cons []map[string]any, peer int) map[string]any {
	t.Helper()
	for _, c := range cons {
		if c["peer_id"] == float64(peer) {
			return c
		}
	}
	t.Fatalf("peer %d not found in %+v", peer, cons)
	return nil
}

func convInt(c map[string]any, key string) int {
	v, _ := c[key].(float64)
	return int(v)
}

// TestConversationsEndpoint exercises the S8 API surface end to end:
// conversation list (boss + employee views), unread counts, open tasks,
// bidirectional threads via ?with=, and the read-cursor endpoint.
func TestConversationsEndpoint(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	// Boss dispatches to W1 (message from NULL + queued task, requested_by=0).
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "ship it"})
	if code != http.StatusCreated {
		t.Fatalf("boss dispatch: %d %v", code, body)
	}
	bossTask := int(body["task_id"].(float64))
	// L1 dispatches to W1 (legal: W1 is in L1's down-chain).
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "from l1"})
	if code != http.StatusCreated {
		t.Fatalf("l1 dispatch: %d", code)
	}
	// W1 reports back to L1 (legal: L1 is on W1's up-chain).
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", w1tok,
		map[string]any{"to": l1, "kind": "report", "body": "done"})
	if code != http.StatusCreated {
		t.Fatalf("w1 report: %d", code)
	}

	// --- W1's conversation list ---
	cons := conversations(t, e, w1tok)
	boss := findConv(t, cons, 0)
	if boss["name"] != "boss" || boss["kind"] != "system" {
		t.Errorf("boss peer meta = %v/%v, want boss/system", boss["name"], boss["kind"])
	}
	if got := convInt(boss, "unread"); got != 1 {
		t.Errorf("w1<->boss unread = %d, want 1", got)
	}
	if got := convInt(boss, "open_tasks"); got != 1 {
		t.Errorf("w1<->boss open_tasks = %d, want 1", got)
	}
	if boss["last_msg_kind"] != "dispatch" {
		t.Errorf("w1<->boss last_msg_kind = %v, want dispatch", boss["last_msg_kind"])
	}
	l1c := findConv(t, cons, l1)
	if got := convInt(l1c, "unread"); got != 1 {
		t.Errorf("w1<->l1 unread = %d, want 1 (the dispatch)", got)
	}
	if got := convInt(l1c, "open_tasks"); got != 1 {
		t.Errorf("w1<->l1 open_tasks = %d, want 1", got)
	}
	if l1c["last_msg_kind"] != "report" { // W1's own report is the newest
		t.Errorf("w1<->l1 last_msg_kind = %v, want report", l1c["last_msg_kind"])
	}

	// --- L1's conversation list sees W1's report as unread ---
	cons = conversations(t, e, l1tok)
	w1c := findConv(t, cons, w1)
	if got := convInt(w1c, "unread"); got != 1 {
		t.Errorf("l1<->w1 unread = %d, want 1 (the report)", got)
	}
	if w1c["last_msg_kind"] != "report" {
		t.Errorf("l1<->w1 last_msg_kind = %v, want report", w1c["last_msg_kind"])
	}

	// --- Boss's conversation list: W1 via outgoing dispatch + open task ---
	cons = conversations(t, e, adminTok)
	bw1 := findConv(t, cons, w1)
	if got := convInt(bw1, "open_tasks"); got != 1 {
		t.Errorf("boss<->w1 open_tasks = %d, want 1", got)
	}
	if got := convInt(bw1, "unread"); got != 0 {
		t.Errorf("boss<->w1 unread = %d, want 0 (nothing addressed to boss)", got)
	}
	if bw1["name"] != "W1" {
		t.Errorf("boss<->w1 name = %v, want W1", bw1["name"])
	}

	// --- Threads ---
	// W1<->boss: only the boss's dispatch, from_id null.
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?with=0", e.srv.URL), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("thread with=0: %d %v", code, body)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("w1<->boss thread = %d msgs, want 1", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	if m0["kind"] != "dispatch" || m0["from_id"] != nil {
		t.Errorf("boss thread row = %v, want dispatch with null from_id", m0)
	}
	// S11: the dispatch message must carry the back-filled task_id so the
	// chat thread can render task chips / action buttons.
	if tid, ok := m0["task_id"]; !ok || int(tid.(float64)) != bossTask {
		t.Errorf("dispatch message task_id = %v, want %d", m0["task_id"], bossTask)
	}

	// W1<->L1: bidirectional, ascending.
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?with=%d", e.srv.URL, l1), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("thread with=l1: %d %v", code, body)
	}
	msgs, _ = body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("w1<->l1 thread = %d msgs, want 2", len(msgs))
	}
	first := msgs[0].(map[string]any)
	second := msgs[1].(map[string]any)
	if first["kind"] != "dispatch" || second["kind"] != "report" {
		t.Errorf("thread order = %v, %v; want dispatch then report", first["kind"], second["kind"])
	}

	// Boss<->W1: boss only sees what it sent (report went to L1).
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?with=%d", e.srv.URL, w1), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("boss thread with=w1: %d %v", code, body)
	}
	msgs, _ = body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["kind"] != "dispatch" {
		t.Fatalf("boss<->w1 thread = %v, want only the dispatch", msgs)
	}

	// --- Scope guards ---
	if code, _ = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?with=%d", e.srv.URL, w1), w1tok, nil); code != http.StatusBadRequest {
		t.Errorf("with=self: got %d, want 400", code)
	}
	if code, _ = req(t, http.MethodGet, e.srv.URL+"/api/messages", w1tok, nil); code != http.StatusBadRequest {
		t.Errorf("no selector: got %d, want 400", code)
	}
	if code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages/read", w1tok,
		map[string]any{"with": w1}); code != http.StatusBadRequest {
		t.Errorf("read with=self: got %d, want 400", code)
	}

	// --- Read cursor ---
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages/read", w1tok,
		map[string]any{"with": 0})
	if code != http.StatusOK {
		t.Fatalf("mark read: %d %v", code, body)
	}
	if body["ok"] != true {
		t.Fatalf("mark read body = %v", body)
	}
	boss = findConv(t, conversations(t, e, w1tok), 0)
	if got := convInt(boss, "unread"); got != 0 {
		t.Errorf("w1<->boss unread after read = %d, want 0", got)
	}
	// L1's report is still unread for W1's l1 conversation (separate cursor).
	l1c = findConv(t, conversations(t, e, w1tok), l1)
	if got := convInt(l1c, "unread"); got != 1 {
		t.Errorf("w1<->l1 unread after boss-read = %d, want 1 (independent cursor)", got)
	}
}

// TestConversationsEmptyAPI: a fresh employee with no traffic gets an empty
// list, not an error.
func TestConversationsEmptyAPI(t *testing.T) {
	e := newEnv(t)
	_, _, _, w1tok := topo(t, e)
	cons := conversations(t, e, w1tok)
	if len(cons) != 0 {
		t.Fatalf("expected no conversations, got %+v", cons)
	}
}

// TestConversationsStatus: AI peers show offline until a heartbeat arrives,
// human peers show manual.
func TestConversationsStatus(t *testing.T) {
	e := newEnv(t)
	dept, err := e.st.CreateDepartment("A", nil)
	if err != nil {
		t.Fatal(err)
	}
	aiID, _ := makeEmployee(t, e, "AI1", store.KindAI, dept)
	humID, _ := makeEmployee(t, e, "H1", store.KindHuman, dept)
	// Boss dispatches to both so they appear in the boss conversation list.
	for _, id := range []int{aiID, humID} {
		if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
			map[string]any{"to": id, "kind": "dispatch", "body": "go"}); code != http.StatusCreated {
			t.Fatalf("boss dispatch to %d: %d", id, code)
		}
	}
	cons := conversations(t, e, adminTok)
	ai, ok := convOf(t, cons, aiID)
	if !ok || ai["status"] != "offline" {
		t.Fatalf("AI status = %v (ok=%v), want offline", ai["status"], ok)
	}
	hum, ok := convOf(t, cons, humID)
	if !ok || hum["status"] != "manual" {
		t.Fatalf("human status = %v (ok=%v), want manual", hum["status"], ok)
	}
	// Heartbeat flips the AI peer online.
	e.api.MarkOnline(aiID, time.Now())
	ai, ok = convOf(t, conversations(t, e, adminTok), aiID)
	if !ok || ai["status"] != "online" {
		t.Fatalf("AI status after heartbeat = %v (ok=%v), want online", ai["status"], ok)
	}
}

// convOf finds a conversation row by peer id (t-fails when absent).
func convOf(t *testing.T, cons []map[string]any, peer int) (map[string]any, bool) {
	t.Helper()
	for _, c := range cons {
		if c["peer_id"] == float64(peer) {
			return c, true
		}
	}
	return nil, false
}
