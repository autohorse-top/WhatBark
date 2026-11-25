package api

import (
	"fmt"
	"net/http"
	"testing"
)

// TestReplyTo (S16): reply_to must reference a message in the same
// conversation (1:1 bidirectional pair or same group), else 400. Covers
// boss/leader/employee directions, group quoting, cross-conversation and
// missing-target rejection, plus the no-reply_to regression path.
func TestReplyTo(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	postMsg := func(tok string, body map[string]any) (int, map[string]any) {
		return req(t, http.MethodPost, e.srv.URL+"/api/messages", tok, body)
	}
	thread := func(tok string, peer int) []map[string]any {
		code, body := req(t, http.MethodGet,
			fmt.Sprintf("%s/api/messages?with=%d", e.srv.URL, peer), tok, nil)
		if code != http.StatusOK {
			t.Fatalf("thread %d: status %d", peer, code)
		}
		raw, _ := body["messages"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, m := range raw {
			out = append(out, m.(map[string]any))
		}
		return out
	}
	find := func(msgs []map[string]any, id int) map[string]any {
		for _, m := range msgs {
			if mid, _ := m["id"].(float64); int(mid) == id {
				return m
			}
		}
		t.Fatalf("message %d not in thread", id)
		return nil
	}

	// --- 1:1 boss <-> worker thread -------------------------------------
	// Boss chats to W1 (from=0 allowed); boss then quotes its own message.
	code, body := postMsg(adminTok, map[string]any{"to": w1, "kind": "chat", "body": "boss original"})
	if code != http.StatusCreated {
		t.Fatalf("boss original: status %d body %v", code, body)
	}
	bossOrig := int(body["message_id"].(float64))
	code, body = postMsg(adminTok, map[string]any{
		"to": w1, "kind": "chat", "body": "> boss original\nboss follow-up", "reply_to": bossOrig,
	})
	if code != http.StatusCreated {
		t.Fatalf("boss reply: status %d body %v", code, body)
	}
	bossReply := int(body["message_id"].(float64))
	row := find(thread(adminTok, w1), bossReply)
	if rt, _ := row["reply_to"].(float64); int(rt) != bossOrig {
		t.Fatalf("boss thread reply_to = %v, want %d (row=%v)", row["reply_to"], bossOrig, row)
	}
	if _, ok := find(thread(adminTok, w1), bossOrig)["reply_to"]; ok {
		t.Fatalf("original message should not expose reply_to")
	}

	// --- 1:1 leader <-> worker thread (task kinds) ----------------------
	// L1 asks W1 (allowed: W1 in Down[L1]; creates a task), W1 reports back
	// quoting that ask message (reverse direction of the same pair).
	code, body = postMsg(l1tok, map[string]any{"to": w1, "kind": "ask", "body": "status?"})
	if code != http.StatusCreated {
		t.Fatalf("l1 ask: status %d body %v", code, body)
	}
	askID := int(body["message_id"].(float64))
	code, body = postMsg(w1tok, map[string]any{
		"to": l1, "kind": "report", "body": "> status?\nall good", "reply_to": askID,
	})
	if code != http.StatusCreated {
		t.Fatalf("w1 report reply: status %d body %v", code, body)
	}
	reportID := int(body["message_id"].(float64))
	row = find(thread(l1tok, w1), reportID)
	if rt, _ := row["reply_to"].(float64); int(rt) != askID {
		t.Fatalf("leader thread reply_to = %v, want %d (row=%v)", row["reply_to"], askID, row)
	}
	// Employee-side view of the same thread also carries reply_to.
	row = find(thread(w1tok, l1), reportID)
	if rt, _ := row["reply_to"].(float64); int(rt) != askID {
		t.Fatalf("employee view reply_to = %v, want %d", row["reply_to"], askID)
	}

	// Cross-conversation: the boss quotes L1's ask message in the (0,W1)
	// thread — different conversation, must 400.
	code, body = postMsg(adminTok, map[string]any{
		"to": w1, "kind": "chat", "body": "boss quotes l1", "reply_to": askID,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("cross-conversation: status %d, want 400 (body %v)", code, body)
	}
	if msg, _ := body["error"].(string); msg != "reply_to does not reference a message in this conversation" {
		t.Fatalf("cross-conversation error = %q", msg)
	}

	// Missing target -> 400.
	code, body = postMsg(adminTok, map[string]any{
		"to": w1, "kind": "chat", "body": "x", "reply_to": 999999,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("missing target: status %d, want 400 (body %v)", code, body)
	}

	// Regression: no reply_to behaves exactly as before.
	code, body = postMsg(adminTok, map[string]any{"to": w1, "kind": "chat", "body": "plain"})
	if code != http.StatusCreated {
		t.Fatalf("plain chat: status %d body %v", code, body)
	}
	if _, ok := find(thread(adminTok, w1), int(body["message_id"].(float64)))["reply_to"]; ok {
		t.Fatalf("plain message should not expose reply_to")
	}

	// --- Group thread -----------------------------------------------------
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/groups", l1tok,
		map[string]any{"name": "s16", "member_ids": []int{w1}})
	if code != http.StatusCreated {
		t.Fatalf("create group: status %d body %v", code, body)
	}
	gid := int(body["id"].(float64))
	code, body = postMsg(w1tok, map[string]any{"group": gid, "kind": "chat", "body": "group original"})
	if code != http.StatusCreated {
		t.Fatalf("group original: status %d body %v", code, body)
	}
	gorig := int(body["message_id"].(float64))
	code, body = postMsg(l1tok, map[string]any{"group": gid, "kind": "chat", "body": "> reply", "reply_to": gorig})
	if code != http.StatusCreated {
		t.Fatalf("group reply: status %d body %v", code, body)
	}
	greply := int(body["message_id"].(float64))
	code, body = req(t, http.MethodGet, fmt.Sprintf("%s/api/messages?group=%d", e.srv.URL, gid), l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("group thread: status %d", code)
	}
	raw, _ := body["messages"].([]any)
	grows := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		grows = append(grows, m.(map[string]any))
	}
	if rt, _ := find(grows, greply)["reply_to"].(float64); int(rt) != gorig {
		t.Fatalf("group thread reply_to = %v, want %d", body["messages"], gorig)
	}

	// Cross-group quoting: a 1:1 message quoted inside a group -> 400.
	code, body = postMsg(w1tok, map[string]any{"group": gid, "kind": "chat", "body": "no", "reply_to": askID})
	if code != http.StatusBadRequest {
		t.Fatalf("cross-group: status %d, want 400 (body %v)", code, body)
	}
}
