package store

import (
	"testing"
	"time"
)

// S33: conv_pins table + PinConversation / UnpinConversation / PinnedPeers.

func TestConvPins(t *testing.T) {
	s := openTestStore(t)
	rootDept, _, alice, bob := seedOrg(t, s)

	pinnedAt := func(viewer, peer int) string {
		t.Helper()
		var at string
		err := s.db.QueryRow(
			`SELECT pinned_at FROM conv_pins WHERE viewer_id = ? AND peer_id = ?`,
			viewer, peer).Scan(&at)
		if err != nil {
			t.Fatalf("read pinned_at (%d,%d): %v", viewer, peer, err)
		}
		return at
	}

	t.Run("empty initially", func(t *testing.T) {
		peers, err := s.PinnedPeers(alice)
		if err != nil {
			t.Fatalf("PinnedPeers: %v", err)
		}
		if len(peers) != 0 {
			t.Fatalf("PinnedPeers = %v, want empty", peers)
		}
	})

	t.Run("pin 1:1 peer", func(t *testing.T) {
		if err := s.PinConversation(alice, bob); err != nil {
			t.Fatalf("PinConversation: %v", err)
		}
		peers, err := s.PinnedPeers(alice)
		if err != nil {
			t.Fatalf("PinnedPeers: %v", err)
		}
		if !peers[bob] {
			t.Fatalf("alice pins: %v, want %d pinned", peers, bob)
		}
		// Per-viewer isolation: bob's view is unaffected.
		other, err := s.PinnedPeers(bob)
		if err != nil {
			t.Fatalf("PinnedPeers(bob): %v", err)
		}
		if len(other) != 0 {
			t.Fatalf("bob pins = %v, want empty", other)
		}
	})

	t.Run("pin boss-side peer 0", func(t *testing.T) {
		if err := s.PinConversation(bob, 0); err != nil {
			t.Fatalf("PinConversation(bob,0): %v", err)
		}
		peers, err := s.PinnedPeers(bob)
		if err != nil {
			t.Fatalf("PinnedPeers: %v", err)
		}
		if !peers[0] {
			t.Fatalf("bob pins = %v, want peer 0 pinned", peers)
		}
	})

	t.Run("pin group uses negative peer convention", func(t *testing.T) {
		gid, err := s.CreateGroup("pin-g", alice, []int{bob}, "")
		if err != nil {
			t.Fatalf("CreateGroup: %v", err)
		}
		gp := -(gid + 1)
		if err := s.PinConversation(alice, gp); err != nil {
			t.Fatalf("PinConversation(group): %v", err)
		}
		peers, err := s.PinnedPeers(alice)
		if err != nil {
			t.Fatalf("PinnedPeers: %v", err)
		}
		if !peers[gp] {
			t.Fatalf("alice pins = %v, want group peer %d pinned", peers, gp)
		}
		if !peers[bob] {
			t.Fatalf("alice pins lost the 1:1 pin: %v", peers)
		}
		other, err := s.PinnedPeers(bob)
		if err != nil {
			t.Fatalf("PinnedPeers(bob): %v", err)
		}
		if other[gp] {
			t.Fatalf("bob pins = %v, must not contain alice's group pin", other)
		}
	})

	t.Run("re-pin refreshes pinned_at", func(t *testing.T) {
		before := pinnedAt(alice, bob)
		time.Sleep(1100 * time.Millisecond) // datetime('now') has 1s resolution
		if err := s.PinConversation(alice, bob); err != nil {
			t.Fatalf("re-pin: %v", err)
		}
		after := pinnedAt(alice, bob)
		if after <= before {
			t.Fatalf("pinned_at not refreshed: before=%s after=%s", before, after)
		}
	})

	t.Run("unpin and idempotent unpin", func(t *testing.T) {
		if err := s.UnpinConversation(alice, bob); err != nil {
			t.Fatalf("UnpinConversation: %v", err)
		}
		peers, err := s.PinnedPeers(alice)
		if err != nil {
			t.Fatalf("PinnedPeers: %v", err)
		}
		if peers[bob] {
			t.Fatalf("alice pins still contain %d after unpin: %v", bob, peers)
		}
		if err := s.UnpinConversation(alice, bob); err != nil {
			t.Fatalf("idempotent unpin: %v", err)
		}
	})

	t.Run("pin without conversation stays invisible to list", func(t *testing.T) {
		// pin-carol has no messages with alice; pinning must not create a
		// conversation row (pin is UI state, not a conversation source).
		carolID, err := s.CreateEmployee("pin-carol", "worker", KindAI, rootDept)
		if err != nil {
			t.Fatalf("CreateEmployee: %v", err)
		}
		if err := s.PinConversation(alice, carolID); err != nil {
			t.Fatalf("PinConversation: %v", err)
		}
		if c, ok := convOf(t, s, alice, carolID); ok {
			t.Fatalf("pinned peer without conversation leaked into list: %+v", c)
		}
	})
}
