package store

import (
	"fmt"
	"strings"
)

// SearchMessages returns messages whose body matches q (case-insensitive for
// ASCII via LIKE, with % and _ treated as literals), restricted to the
// conversations participant is part of: 1:1 messages they sent or received,
// plus group messages of groups they belong to. Newest first, capped at
// limit (default 20, hard max 50). A negative participant means the boss:
// all 1:1 messages plus group messages of groups the boss is a member of —
// a non-member group thread would 403 on open, so hits must stay openable
// (S32/M1).
func (s *Store) SearchMessages(q string, participant, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	like := "%" + escapeLike(q) + "%"
	base := `
		SELECT id, from_id, to_id, kind, body, project_id, task_id, reply_to, group_id, created_at
		FROM messages
		WHERE body LIKE ? ESCAPE '\'`
	var args []any
	args = append(args, like)
	if participant >= 0 {
		base += `
		  AND (to_id = ? OR COALESCE(from_id, 0) = ?
		       OR group_id IN (SELECT group_id FROM group_members WHERE employee_id = ?))`
		args = append(args, participant, participant, participant)
	} else {
		// Boss: every 1:1 message (group_id IS NULL — all 1:1s involve the
		// boss, and employee-to-employee threads are out of the boss's thread
		// surface anyway) plus group messages of groups the boss belongs to.
		base += `
		  AND (group_id IS NULL
		       OR group_id IN (SELECT group_id FROM group_members WHERE employee_id = 0))`
	}
	base += `
		ORDER BY id DESC
		LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(base, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search messages: %w", err)
	}
	return scanMessages(rows, "store: search messages")
}

// SearchMentions returns messages that (a) name viewerID among their mention
// targets (message_mentions, S20), (b) contain q in the body (case-insensitive
// for ASCII via LIKE, % and _ literal via escapeLike), and (c) are visible to
// the viewer — the same visibility rule as SearchMessages:
//   - employee (viewerID > 0): 1:1 messages they sent or received, plus group
//     messages of groups they belong to;
//   - boss (viewerID = 0): all 1:1 messages, plus group messages of groups the
//     boss is a member of.
//
// kind='system' is excluded (system events never mention). Newest first,
// capped at limit (default 20, hard max 50). S36, v1.2.x item 13.
//
// Convention: viewerID 0 = boss, viewerID > 0 = employee id. Unlike
// SearchMessages (where participant < 0 means boss), negative values are NOT
// a valid caller argument here; callers must normalize the identity first.
func (s *Store) SearchMentions(q string, viewerID, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	like := "%" + escapeLike(q) + "%"
	base := `
		SELECT id, from_id, to_id, kind, body, project_id, task_id, reply_to, group_id, created_at
		FROM messages m
		WHERE m.body LIKE ? ESCAPE '\'
		  AND m.kind <> 'system'
		  AND m.id IN (SELECT message_id FROM message_mentions WHERE employee_id = ?)`
	var args []any
	args = append(args, like, viewerID)
	if viewerID > 0 {
		base += `
		  AND ((m.group_id IS NULL AND (m.to_id = ? OR COALESCE(m.from_id, 0) = ?))
		       OR (m.group_id IS NOT NULL
		           AND m.group_id IN (SELECT group_id FROM group_members WHERE employee_id = ?)))`
		args = append(args, viewerID, viewerID, viewerID)
	} else {
		// Boss: every 1:1 message plus group messages of groups the boss
		// belongs to. (In practice the boss is only ever a mention target in
		// group messages — 1:1 sends are boss-originated and the sender is
		// never its own mention target — but the rule matches SearchMessages.)
		base += `
		  AND (m.group_id IS NULL
		       OR m.group_id IN (SELECT group_id FROM group_members WHERE employee_id = 0))`
	}
	base += `
		ORDER BY m.id DESC
		LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(base, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search mentions: %w", err)
	}
	return scanMessages(rows, "store: search mentions")
}

// GroupSearchHit is one row of the group-name search (S32, v1.2.x item 9):
// a group/channel the caller is a member of, matched by name.
type GroupSearchHit struct {
	GroupID     int
	Name        string
	Kind        string // "group" | "channel"
	MemberCount int
}

// SearchGroupNames returns groups/channels whose name contains q
// (case-insensitive for ASCII via LIKE; % and _ are literal, see escapeLike),
// restricted to groups the caller is a member of — the same visibility as
// ListGroupConversations, so every hit is openable (non-members would 403
// in the thread). viewerID 0 is the boss (a member like any other). Newest
// groups first, capped at limit (default 20, hard max 50).
func (s *Store) SearchGroupNames(q string, viewerID, limit int) ([]GroupSearchHit, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	like := "%" + escapeLike(q) + "%"
	rows, err := s.db.Query(`
		SELECT g.id, g.name, g.kind,
		       (SELECT COUNT(*) FROM group_members gm WHERE gm.group_id = g.id) AS member_count
		FROM groups g
		WHERE g.name LIKE ? ESCAPE '\'
		  AND EXISTS (SELECT 1 FROM group_members gm
		              WHERE gm.group_id = g.id AND gm.employee_id = ?)
		ORDER BY g.id DESC
		LIMIT ?`, like, viewerID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search group names: %w", err)
	}
	defer rows.Close()
	out := make([]GroupSearchHit, 0, 8)
	for rows.Next() {
		var h GroupSearchHit
		if err := rows.Scan(&h.GroupID, &h.Name, &h.Kind, &h.MemberCount); err != nil {
			return nil, fmt.Errorf("store: search group names: scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search group names: %w", err)
	}
	return out, nil
}

// escapeLike neutralizes LIKE wildcards in user input so q matches literally.
func escapeLike(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `%`, `\%`)
	v = strings.ReplaceAll(v, `_`, `\_`)
	return v
}
