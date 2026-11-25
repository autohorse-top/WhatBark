package api

import (
	"fmt"
	"net/http"
	"testing"
)

// S33: POST /api/conversations/{peer_id}/pin + the pinned flag / ordering in
// GET /api/conversations.

// pin calls the S33 pin endpoint with a boolean payload.
func pin(t *testing.T, e *env, tok string, peerID int, v bool) (int, map[string]any) {
	t.Helper()
	return req(t, http.MethodPost,
		fmt.Sprintf("%s/api/conversations/%d/pin", e.srv.URL, peerID),
		tok, map[string]any{"pinned": v})
}

func TestSetConvPinAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)
	l2tok := "tok-L2"

	// Seed two 1:1 threads for W1: boss->W1 (older), L1->W1 (newer).
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "boss to w1"}); code != http.StatusCreated {
		t.Fatalf("boss->w1: got %d, want 201", code)
	}
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "l1 to w1"}); code != http.StatusCreated {
		t.Fatalf("l1->w1: got %d, want 201", code)
	}

	// Baseline: activity order puts L1 (newest) first, boss second.
	cons := conversations(t, e, w1tok)
	if len(cons) != 2 {
		t.Fatalf("w1 conversations = %d, want 2: %v", len(cons), cons)
	}
	for _, row := range cons {
		if _, ok := row["pinned"]; !ok {
			t.Fatalf("row %v missing pinned key", row)
		}
	}
	if cons[0]["peer_id"] != float64(l1) || cons[1]["peer_id"] != float64(0) {
		t.Fatalf("baseline order = %v/%v, want l1 then boss", cons[0]["peer_id"], cons[1]["peer_id"])
	}

	t.Run("pin 1:1 boss-side peer 0", func(t *testing.T) {
		code, body := pin(t, e, w1tok, 0, true)
		if code != http.StatusOK {
			t.Fatalf("pin 0: got %d %v, want 200", code, body)
		}
		if body["ok"] != true || body["peer_id"] != float64(0) || body["pinned"] != true {
			t.Fatalf("pin 0 body = %v", body)
		}
		cons := conversations(t, e, w1tok)
		if cons[0]["peer_id"] != float64(0) || cons[0]["pinned"] != true {
			t.Fatalf("first row = %v, want pinned boss row", cons[0])
		}
		if cons[1]["peer_id"] != float64(l1) || cons[1]["pinned"] != false {
			t.Fatalf("second row = %v, want unpinned l1", cons[1])
		}
	})

	t.Run("pinning is per-viewer", func(t *testing.T) {
		for name, tok := range map[string]string{"boss": adminTok, "l1": l1tok} {
			c := conversations(t, e, tok)
			row := findConv(t, c, w1)
			if row["pinned"] != false {
				t.Fatalf("%s view: w1 row pinned = %v, want false", name, row["pinned"])
			}
		}
	})

	t.Run("new non-pinned message does not jump ahead of pinned", func(t *testing.T) {
		// L1->W1 again: L1 becomes most-recent, but boss stays first (pinned).
		if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
			map[string]any{"to": w1, "kind": "dispatch", "body": "l1 again"}); code != http.StatusCreated {
			t.Fatalf("l1->w1 again: got %d", code)
		}
		cons := conversations(t, e, w1tok)
		if cons[0]["peer_id"] != float64(0) {
			t.Fatalf("pinned row not first after new message: %v", cons[0])
		}
		if cons[1]["last_msg_body"] != "l1 again" {
			t.Fatalf("non-pinned order broken: %v", cons[1])
		}
	})

	t.Run("unpin restores activity order and is idempotent", func(t *testing.T) {
		if code, _ := pin(t, e, w1tok, 0, false); code != http.StatusOK {
			t.Fatalf("unpin: got %d, want 200", code)
		}
		if code, _ := pin(t, e, w1tok, 0, false); code != http.StatusOK {
			t.Fatalf("idempotent unpin: got %d, want 200", code)
		}
		cons := conversations(t, e, w1tok)
		if cons[0]["peer_id"] != float64(l1) {
			t.Fatalf("first row after unpin = %v, want l1 (newest)", cons[0])
		}
		if cons[0]["pinned"] != false {
			t.Fatalf("pinned flag after unpin = %v, want false", cons[0]["pinned"])
		}
	})

	t.Run("self-pin is 400", func(t *testing.T) {
		if code, _ := pin(t, e, w1tok, w1, true); code != http.StatusBadRequest {
			t.Fatalf("self-pin: got %d, want 400", code)
		}
		// Boss (emp 0) pinning the boss side (peer 0) is also self-pin -> 400.
		if code, _ := pin(t, e, adminTok, 0, true); code != http.StatusBadRequest {
			t.Fatalf("boss self-pin (peer 0): got %d, want 400", code)
		}
	})

	t.Run("unknown employee is 400", func(t *testing.T) {
		if code, _ := pin(t, e, w1tok, 9999, true); code != http.StatusBadRequest {
			t.Fatalf("unknown employee: got %d, want 400", code)
		}
	})

	t.Run("group pin: member 200, non-member 403, unknown 404", func(t *testing.T) {
		gid, err := e.st.CreateGroup("pin-squad", w1, []int{l1}, "group")
		if err != nil {
			t.Fatalf("CreateGroup: %v", err)
		}
		gp := -(gid + 1)

		// Creator w1 pins the group: 200 + row pinned at top of w1's list.
		code, body := pin(t, e, w1tok, gp, true)
		if code != http.StatusOK {
			t.Fatalf("group pin: got %d %v, want 200", code, body)
		}
		if body["peer_id"] != float64(gp) || body["pinned"] != true {
			t.Fatalf("group pin body = %v", body)
		}
		cons := conversations(t, e, w1tok)
		row := findConv(t, cons, gp)
		if row["pinned"] != true || row["is_group"] != true || row["kind"] != "group" {
			t.Fatalf("group row = %v, want pinned group", row)
		}
		if cons[0]["peer_id"] != float64(gp) {
			t.Fatalf("group pin not first: %v", cons[0])
		}

		// L2 is not a member of pin-squad -> 403.
		if code, _ := pin(t, e, l2tok, gp, true); code != http.StatusForbidden {
			t.Fatalf("non-member group pin: got %d, want 403", code)
		}
		// S33 spec: boss (emp 0) is exempt from the group membership check,
		// even when not a member of pin-squad -> 200.
		if code, body := pin(t, e, adminTok, gp, true); code != http.StatusOK {
			t.Fatalf("boss non-member group pin: got %d %v, want 200 (boss exempt)", code, body)
		}
		// Unknown group -> 404.
		if code, _ := pin(t, e, w1tok, -9999-1, true); code != http.StatusNotFound {
			t.Fatalf("unknown group: got %d, want 404", code)
		}
	})

	t.Run("body validation", func(t *testing.T) {
		url := fmt.Sprintf("%s/api/conversations/%d/pin", e.srv.URL, l1)
		if code, _ := req(t, http.MethodPost, url, w1tok, map[string]any{}); code != http.StatusBadRequest {
			t.Fatalf("missing pinned: got %d, want 400", code)
		}
		if code, _ := req(t, http.MethodPost, url, w1tok, map[string]any{"pinned": "yes"}); code != http.StatusBadRequest {
			t.Fatalf("non-bool pinned: got %d, want 400", code)
		}
		if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/conversations/abc/pin",
			w1tok, map[string]any{"pinned": true}); code != http.StatusBadRequest {
			t.Fatalf("bad peer_id: got %d, want 400", code)
		}
	})
}

// TestSetConvPinChannel: a channel (kind=channel) is pinnable by a member
// exactly like a group.
func TestSetConvPinChannel(t *testing.T) {
	e := newEnv(t)
	_, _, w1, w1tok := topo(t, e)
	gid, err := e.st.CreateGroup("pin-chan", 0, []int{w1}, "channel")
	if err != nil {
		t.Fatalf("CreateGroup channel: %v", err)
	}
	gp := -(gid + 1)
	if code, _ := pin(t, e, w1tok, gp, true); code != http.StatusOK {
		t.Fatalf("channel pin: got %d, want 200", code)
	}
	cons := conversations(t, e, w1tok)
	row := findConv(t, cons, gp)
	if row["pinned"] != true || row["kind"] != "channel" || row["is_group"] != true {
		t.Fatalf("channel row = %v, want pinned channel", row)
	}
}
