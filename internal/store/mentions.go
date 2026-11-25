package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
)

// mentionRe matches @tokens in message bodies. The character set mirrors the
// frontend highlight pattern (S11). A token is a mention only when it
// resolves (via EmployeeIDByName, case-sensitive) to an employee that is a
// participant of the conversation and is not the sender.
var mentionRe = regexp.MustCompile(`@([A-Za-z0-9_-]{1,64})`)

// EmployeeIDByName returns the id of the employee whose name equals name
// exactly (case-sensitive BINARY collation). ok is false when no such
// employee exists.
func (s *Store) EmployeeIDByName(name string) (int, bool, error) {
	var id int
	err := s.db.QueryRow(`SELECT id FROM employees WHERE name = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: employee by name %q: %w", name, err)
	}
	return id, true, nil
}

// MentionIDsIn extracts real-mention targets from body.
//
// A matched @token becomes a mention when:
//  1. it resolves via EmployeeIDByName (case-sensitive) to an employee id,
//     OR the literal token "boss" is accepted when participants contains 0
//     (S20 ruling: the boss has employee_id 0 and no employees row, so
//     EmployeeIDByName can never return 0; only the exact word "boss" maps
//     to the boss);
//  2. the resolved id is in participants;
//  3. the resolved id differs from sender (self-mentions are ignored).
//
// Results are de-duplicated and sorted ascending by id. Unrecognised or
// non-participant tokens are silently dropped.
func (s *Store) MentionIDsIn(body string, participants []int, sender int) ([]int, error) {
	if body == "" {
		return []int{}, nil
	}
	part := make(map[int]bool, len(participants))
	for _, p := range participants {
		part[p] = true
	}
	seen := make(map[int]bool)
	out := []int{}
	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		token := m[1]
		var cands []int
		if id, ok, err := s.EmployeeIDByName(token); err != nil {
			return nil, err
		} else if ok {
			cands = append(cands, id)
		}
		// S20: when the boss (id 0) is a participant, the literal token
		// "boss" (case-sensitive) maps to 0.
		if token == "boss" && part[0] {
			cands = append(cands, 0)
		}
		for _, id := range cands {
			if part[id] && id != sender && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Ints(out)
	return out, nil
}

// AddMessageMentions records mention targets for a message. Duplicates are
// silently ignored (composite primary key). The message must exist
// (enforced by the ON DELETE CASCADE FK).
func (s *Store) AddMessageMentions(msgID int, empIDs []int) error {
	for _, id := range empIDs {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO message_mentions (message_id, employee_id) VALUES (?, ?)`,
			msgID, id,
		); err != nil {
			return fmt.Errorf("store: add message mentions %d: %w", msgID, err)
		}
	}
	return nil
}

// MessageMentions returns the mentioned employee ids for a message,
// ascending. An unknown message id yields an empty slice.
func (s *Store) MessageMentions(msgID int) ([]int, error) {
	rows, err := s.db.Query(
		`SELECT employee_id FROM message_mentions WHERE message_id = ? ORDER BY employee_id`, msgID)
	if err != nil {
		return nil, fmt.Errorf("store: message mentions %d: %w", msgID, err)
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: message mentions %d: %w", msgID, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MentionedUnread returns the count of 1:1 messages addressed to viewerID,
// sent by peerID, beyond the viewer's read cursor for that peer, that
// mention the viewer. peerID 0 is the boss. The cursor defaults to 0 when
// the viewer has never read the conversation.
func (s *Store) MentionedUnread(viewerID, peerID int) (int, error) {
	cursors, err := s.ReadCursors(viewerID)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM messages m
		WHERE m.to_id = ?
		  AND COALESCE(m.from_id, 0) = ?
		  AND m.id > ?
		  AND EXISTS (
			SELECT 1 FROM message_mentions mm
			WHERE mm.message_id = m.id AND mm.employee_id = ?
		  )`,
		viewerID, peerID, cursors[peerID], viewerID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: mentioned unread %d/%d: %w", viewerID, peerID, err)
	}
	return n, nil
}

// GroupMentionedUnread returns the count of group messages in groupID that
// mention viewerID, excluding system messages and messages sent by the
// viewer themselves, beyond the viewer's read cursor for the group.
// Returns 0 when the viewer is not a member of the group.
func (s *Store) GroupMentionedUnread(viewerID, groupID int) (int, error) {
	// Non-members have no mention-unread in that group.
	isMember, err := s.IsGroupMember(groupID, viewerID)
	if err != nil {
		return 0, err
	}
	if !isMember {
		return 0, nil
	}
	cursors, err := s.ReadCursors(viewerID)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM messages m
		WHERE m.group_id = ?
		  AND m.kind <> 'system'
		  AND COALESCE(m.from_id, 0) <> ?
		  AND m.id > ?
		  AND EXISTS (
			SELECT 1 FROM message_mentions mm
			WHERE mm.message_id = m.id AND mm.employee_id = ?
		  )`,
		groupID, viewerID, cursors[groupPeerID(groupID)], viewerID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: group mentioned unread %d/%d: %w", viewerID, groupID, err)
	}
	return n, nil
}
