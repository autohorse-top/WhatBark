package store

import (
	"fmt"
)

// S33: per-viewer conversation pins (pin to top of the conversation list).
//
// peer_id follows the conv_reads convention: >= 0 is a 1:1 peer (0 = the
// boss/system side), negative values are groups (-(group_id+1)). Pins are
// per-viewer UI state: they never affect message delivery or read state, and
// a pin on a peer with no existing conversation simply stays invisible in
// ListConversations until messages (or an open task) link the two.

// PinConversation pins peerID for viewerID. Re-pinning an already-pinned
// peer refreshes pinned_at (the API layer only calls this when the flag
// transitions false -> true, so this stays a simple upsert).
func (s *Store) PinConversation(viewerID, peerID int) error {
	_, err := s.db.Exec(`
		INSERT INTO conv_pins (viewer_id, peer_id, pinned_at)
		VALUES (?, ?, datetime('now'))
		ON CONFLICT (viewer_id, peer_id) DO UPDATE SET
			pinned_at = datetime('now')`, viewerID, peerID)
	if err != nil {
		return fmt.Errorf("store: pin conversation: %w", err)
	}
	return nil
}

// UnpinConversation removes viewerID's pin on peerID. Idempotent: a missing
// row is a no-op, not an error.
func (s *Store) UnpinConversation(viewerID, peerID int) error {
	if _, err := s.db.Exec(
		`DELETE FROM conv_pins WHERE viewer_id = ? AND peer_id = ?`,
		viewerID, peerID); err != nil {
		return fmt.Errorf("store: unpin conversation: %w", err)
	}
	return nil
}

// PinnedPeers returns the set of peer IDs viewerID has pinned (empty map
// when none).
func (s *Store) PinnedPeers(viewerID int) (map[int]bool, error) {
	out := map[int]bool{}
	rows, err := s.db.Query(`SELECT peer_id FROM conv_pins WHERE viewer_id = ?`, viewerID)
	if err != nil {
		return nil, fmt.Errorf("store: pinned peers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: pinned peers: %w", err)
		}
		out[p] = true
	}
	return out, rows.Err()
}
