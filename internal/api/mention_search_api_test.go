package api

import (
	"net/http"
	"testing"
)

// TestSearchMentionsAPI covers S36 (v1.2.x item 13): the "mentions" facet of
// GET /api/search — messages that @ the caller and match q, with the same
// visibility scoping as the messages facet. The facet must always appear as
// an array (empty when no hits), and the existing facets must be unchanged.
func TestSearchMentionsAPI(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)

	postMsg := func(tok string, body map[string]any) int {
		t.Helper()
		code, resp := req(t, http.MethodPost, e.srv.URL+"/api/messages", tok, body)
		if code != http.StatusCreated {
			t.Fatalf("POST /api/messages %v: got %d, want 201 (body %v)", body, code, resp)
		}
		return int(resp["message_id"].(float64))
	}
	search := func(tok, q string) (int, map[string]any) {
		t.Helper()
		return req(t, http.MethodGet, e.srv.URL+"/api/search?q="+q, tok, nil)
	}
	mentionsOf := func(t *testing.T, body map[string]any) []map[string]any {
		t.Helper()
		raw, _ := body["mentions"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, m := range raw {
			out = append(out, m.(map[string]any))
		}
		return out
	}

	// s36grp: members l1 (creator) + w1; s36bgrp: boss (creator) + w1.
	gid := makeGroup(t, e, l1tok, "s36grp", []int{w1})
	bg := makeGroup(t, e, adminTok, "s36bgrp", []int{w1})

	mG := postMsg(w1tok, map[string]any{"group": gid, "kind": "chat", "body": "@L1 s36alpha"})
	mB2 := postMsg(w1tok, map[string]any{"group": bg, "kind": "chat", "body": "@boss s36beta"})
	mB1 := postMsg(adminTok, map[string]any{"to": w1, "kind": "chat", "body": "@W1 s36gamma"})

	// 1) l1: exactly one mention hit (the group message that @'d l1), with
	// group_id; the same row also appears in the messages facet (expected
	// overlap); other facets stay present.
	code, body := search(l1tok, "s36alpha")
	if code != http.StatusOK {
		t.Fatalf("l1 search: got %d, want 200", code)
	}
	ms := mentionsOf(t, body)
	if len(ms) != 1 || int(ms[0]["id"].(float64)) != mG {
		t.Fatalf("l1 mentions = %v, want exactly [%d]", body["mentions"], mG)
	}
	if gidGot, ok := ms[0]["group_id"].(float64); !ok || int(gidGot) != gid {
		t.Fatalf("l1 mention row group_id = %v, want %d", ms[0]["group_id"], gid)
	}
	msgsRaw, _ := body["messages"].([]any)
	found := false
	for _, m := range msgsRaw {
		if int(m.(map[string]any)["id"].(float64)) == mG {
			found = true
		}
	}
	if !found {
		t.Fatalf("l1 messages facet lost the group row %d: %v", mG, body["messages"])
	}
	if _, ok := body["contacts"].([]any); !ok {
		t.Fatalf("contacts facet missing/invalid: %v", body["contacts"])
	}
	if _, ok := body["groups"].([]any); !ok {
		t.Fatalf("groups facet missing/invalid: %v", body["groups"])
	}

	// 2) w1 (the sender): mentions is an empty array; the row stays visible
	// in the messages facet (w1 is a group member).
	code, body = search(w1tok, "s36alpha")
	if code != http.StatusOK {
		t.Fatalf("w1 search: got %d, want 200", code)
	}
	if ms := mentionsOf(t, body); len(ms) != 0 {
		t.Fatalf("w1 (sender) sees own-sent mention row: %v", ms)
	}
	msgsRaw, _ = body["messages"].([]any)
	if len(msgsRaw) < 1 {
		t.Fatalf("w1 messages facet should still see the group row: %v", body["messages"])
	}

	// 3) w1: boss 1:1 @w1 — one mention, no group_id, no from_id (boss has
	// no sender row).
	code, body = search(w1tok, "s36gamma")
	if code != http.StatusOK {
		t.Fatalf("w1 search gamma: got %d, want 200", code)
	}
	ms = mentionsOf(t, body)
	if len(ms) != 1 || int(ms[0]["id"].(float64)) != mB1 {
		t.Fatalf("w1 mentions = %v, want exactly [%d]", body["mentions"], mB1)
	}
	if _, ok := ms[0]["group_id"]; ok {
		t.Fatalf("1:1 mention row must not carry group_id: %v", ms[0])
	}
	if _, ok := ms[0]["from_id"]; ok {
		t.Fatalf("boss-sent row must not carry from_id: %v", ms[0])
	}

	// 4) boss: @boss in the boss's group — one mention with group_id.
	code, body = search(adminTok, "s36beta")
	if code != http.StatusOK {
		t.Fatalf("boss search beta: got %d, want 200", code)
	}
	ms = mentionsOf(t, body)
	if len(ms) != 1 || int(ms[0]["id"].(float64)) != mB2 {
		t.Fatalf("boss mentions = %v, want exactly [%d]", body["mentions"], mB2)
	}
	if gidGot, ok := ms[0]["group_id"].(float64); !ok || int(gidGot) != bg {
		t.Fatalf("boss mention row group_id = %v, want %d", ms[0]["group_id"], bg)
	}

	// 5) boss: not mentioned in s36grp -> zero hits (visibility is moot,
	// the mention subquery already filters out).
	code, body = search(adminTok, "s36alpha")
	if code != http.StatusOK {
		t.Fatalf("boss search alpha: got %d, want 200", code)
	}
	if ms := mentionsOf(t, body); len(ms) != 0 {
		t.Fatalf("boss sees l1's mention row: %v", ms)
	}

	// 6) validation regression: q<2 and out-of-range limit still 400.
	for _, q := range []string{"a", ""} {
		if code, _ := search(l1tok, q); code != http.StatusBadRequest {
			t.Fatalf("q=%q: got %d, want 400", q, code)
		}
	}
	for _, lim := range []string{"0", "51", "-1", "x"} {
		code, _ := req(t, http.MethodGet, e.srv.URL+"/api/search?q=s36alpha&limit="+lim, l1tok, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("limit=%s: got %d, want 400", lim, code)
		}
	}
}
