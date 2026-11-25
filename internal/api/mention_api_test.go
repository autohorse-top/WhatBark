package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"whatbark/internal/store"
)

// mentionsOf extracts the "mentions" int array from a message row
// (always an array — empty when the message has no real mentions).
func mentionsOf(t *testing.T, row map[string]any) []int {
	t.Helper()
	raw, _ := row["mentions"].([]any)
	out := make([]int, 0, len(raw))
	for _, v := range raw {
		out = append(out, int(v.(float64)))
	}
	return out
}

// TestMentionAPIOneToOne (S20): a 1:1 chat with a real @mention exposes
// mentions[] on the message row and unread_mentioned on the peer's
// conversation row; marking the thread read clears both; tokens that are
// not participants (or unknown) produce no mention.
func TestMentionAPIOneToOne(t *testing.T) {
	e := newEnv(t)
	a, err := e.st.CreateDepartment("A", nil)
	if err != nil {
		t.Fatal(err)
	}
	l1, l1tok := makeEmployee(t, e, "L1", store.KindAI, a)
	if err := e.st.SetDepartmentLeader(a, l1); err != nil {
		t.Fatal(err)
	}
	b, err := e.st.CreateDepartment("B", &a)
	if err != nil {
		t.Fatal(err)
	}
	l2, l2tok := makeEmployee(t, e, "L2", store.KindAI, b)
	if err := e.st.SetDepartmentLeader(b, l2); err != nil {
		t.Fatal(err)
	}
	// W1 exists but is NOT a participant in the L1<->L2 thread: used to
	// prove "existing employee, non-participant" tokens are dropped (the
	// @W1 token resolves by name to this employee, who is out of thread).
	_, w1tok := makeEmployee(t, e, "W1", store.KindAI, b)

	// L1 -> L2 chat (leader-to-leader) mentioning the recipient.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": l2, "kind": "chat", "body": "@L2 你好"})
	if code != http.StatusCreated {
		t.Fatalf("L1->L2 chat: got %d %v, want 201", code, body)
	}
	mid := int(body["message_id"].(float64))

	// L2's conversation row: unread_mentioned=1 (and unread=1).
	cons := conversations(t, e, l2tok)
	peer := findConv(t, cons, l1)
	if got := convInt(peer, "unread_mentioned"); got != 1 {
		t.Fatalf("L2 unread_mentioned = %d, want 1", got)
	}
	if got := convInt(peer, "unread"); got != 1 {
		t.Errorf("L2 unread = %d, want 1", got)
	}

	// Thread row carries mentions=[l2].
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?with=%d", e.srv.URL, l1), l2tok, nil)
	if code != http.StatusOK {
		t.Fatalf("thread: got %d", code)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("thread = %d msgs, want 1", len(msgs))
	}
	row := msgs[0].(map[string]any)
	if row["id"] != float64(mid) {
		t.Fatalf("thread row id = %v, want %d", row["id"], mid)
	}
	if got := mentionsOf(t, row); !slices.Equal(got, []int{l2}) {
		t.Errorf("mentions = %v, want [%d]", got, l2)
	}

	// Non-participant token ("@W1") and unknown token ("@nobody"): no
	// mentions, no new unread_mentioned.
	for _, txt := range []string{"@W1 在吗", "@nobody 在吗"} {
		if code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
			map[string]any{"to": l2, "kind": "chat", "body": txt}); code != http.StatusCreated {
			t.Fatalf("chat %q: got %d %v, want 201", txt, code, body)
		}
	}
	peer = findConv(t, conversations(t, e, l2tok), l1)
	if got := convInt(peer, "unread_mentioned"); got != 1 {
		t.Fatalf("unread_mentioned after non-mention chats = %d, want 1", got)
	}
	if got := convInt(peer, "unread"); got != 3 {
		t.Fatalf("unread = %d, want 3", got)
	}
	// W1 never participated: its conversation list stays empty.
	if cws := conversations(t, e, w1tok); len(cws) != 0 {
		t.Errorf("W1 conversations = %d rows, want 0 (non-participant)", len(cws))
	}

	// Mark read clears both counters.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages/read", l2tok,
		map[string]any{"with": l1}); code != http.StatusOK {
		t.Fatalf("mark read: got %d", code)
	}
	peer = findConv(t, conversations(t, e, l2tok), l1)
	if got := convInt(peer, "unread_mentioned"); got != 0 {
		t.Errorf("unread_mentioned after read = %d, want 0", got)
	}
	if got := convInt(peer, "unread"); got != 0 {
		t.Errorf("unread after read = %d, want 0", got)
	}
}

// TestMentionAPIGroup (S20): a group @mention exposes mentions[] on the
// group message row and unread_mentioned on the group conversation row;
// marking the group read clears it.
func TestMentionAPIGroup(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)
	gid := makeGroup(t, e, l1tok, "squad", []int{w1})

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gid, "kind": "chat", "body": "@W1 在吗"})
	if code != http.StatusCreated {
		t.Fatalf("group chat: got %d %v, want 201", code, body)
	}
	mid := int(body["message_id"].(float64))

	// W1's group row: unread_mentioned=1, unread=1 (system creation msg
	// never counts as unread).
	cons := conversations(t, e, w1tok)
	gc := findConv(t, cons, -(gid + 1))
	if got := convInt(gc, "unread_mentioned"); got != 1 {
		t.Fatalf("group unread_mentioned = %d, want 1", got)
	}
	if got := convInt(gc, "unread"); got != 1 {
		t.Errorf("group unread = %d, want 1", got)
	}

	// Group thread row carries mentions=[w1] (second row: system + chat).
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gid), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("group thread: got %d", code)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("group thread = %d msgs, want 2", len(msgs))
	}
	row := msgs[1].(map[string]any)
	if row["id"] != float64(mid) {
		t.Fatalf("thread row id = %v, want %d", row["id"], mid)
	}
	if got := mentionsOf(t, row); !slices.Equal(got, []int{w1}) {
		t.Errorf("mentions = %v, want [%d]", got, w1)
	}

	// Mark the group read -> both counters clear.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages/read", w1tok,
		map[string]any{"group": gid}); code != http.StatusOK {
		t.Fatalf("group mark read: got %d", code)
	}
	gc = findConv(t, conversations(t, e, w1tok), -(gid + 1))
	if got := convInt(gc, "unread_mentioned"); got != 0 {
		t.Errorf("group unread_mentioned after read = %d, want 0", got)
	}
	if got := convInt(gc, "unread"); got != 0 {
		t.Errorf("group unread after read = %d, want 0", got)
	}
}

// TestMentionAPIBossTarget (S20): the boss (id 0) is a legal mention target
// via the literal token "boss" when the boss is a participant.
// NOTE: a literal 1:1 employee->boss send is 403 by routing (to<=0 is
// rejected, per TASK.md S8 decision "1:1 follows the real topology only;
// the boss exception allows inbound only"),
// so the store-level boss mention path is covered by
// TestMentionIDsIn (store). Here we verify the two reachable boss-facing
// paths: (a) boss -> employee 1:1 chat that mentions the employee, and
// (b) an employee group message that mentions the boss.
func TestMentionAPIBossTarget(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)

	// (a) boss -> W1 1:1 chat that mentions W1 (body "@W1 ..." in the
	// fixture below): W1's peer-0 row gets unread_mentioned=1; the thread
	// row carries mentions=[w1].
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": w1, "kind": "chat", "body": "@W1 任务来了"})
	if code != http.StatusCreated {
		t.Fatalf("boss chat: got %d %v, want 201", code, body)
	}
	cons := conversations(t, e, w1tok)
	bossRow := findConv(t, cons, 0)
	if got := convInt(bossRow, "unread_mentioned"); got != 1 {
		t.Fatalf("W1 boss-row unread_mentioned = %d, want 1", got)
	}
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/messages?with=0", w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("boss thread: got %d", code)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("boss thread = %d msgs, want 1", len(msgs))
	}
	row := msgs[0].(map[string]any)
	if got := mentionsOf(t, row); !slices.Equal(got, []int{w1}) {
		t.Errorf("boss chat mentions = %v, want [%d]", got, w1)
	}

	// (b) group with the boss: L1 posts a message mentioning the boss ->
	// the boss's group row gets unread_mentioned=1 and carries mentions
	// containing 0.
	gid := makeGroup(t, e, l1tok, "boss-squad", []int{0})
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gid, "kind": "chat", "body": "@boss 在吗"})
	if code != http.StatusCreated {
		t.Fatalf("group @boss: got %d, want 201", code)
	}
	cons = conversations(t, e, adminTok)
	gc := findConv(t, cons, -(gid + 1))
	if got := convInt(gc, "unread_mentioned"); got != 1 {
		t.Fatalf("boss group unread_mentioned = %d, want 1", got)
	}
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gid), adminTok, nil)
	msgs, _ = body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("boss group thread = %d msgs, want 2", len(msgs))
	}
	row = msgs[1].(map[string]any)
	if got := mentionsOf(t, row); !slices.Contains(got, 0) {
		t.Errorf("@boss mentions = %v, want to contain 0", got)
	}
}
