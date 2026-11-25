package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrNoNewGroupMembers is returned by AddGroupMembers when the caller passes
// no members or only members that are already in the group. Callers that
// treat "already a member" as a no-op success can match it with errors.Is.
var ErrNoNewGroupMembers = errors.New("no new members to add")

// Group is a group chat (S14). CreatedBy 0 means the boss created it.
// Kind (S21) is "group" (default) or "channel" (project channel, boss-only).
// S30 policy: DefaultPriority is applied to group dispatches that omit an
// explicit priority; SlaTimeoutSeconds (0 = disabled) makes the scheduler
// post a one-time system reminder when a group task stays open longer.
type Group struct {
	ID                int
	Name              string
	CreatedBy         int
	CreatedAt         string
	Kind              string
	DefaultPriority   int
	SlaTimeoutSeconds int
	// WebhookURL (V-4): optional IM-channel webhook; task-terminal and
	// mention events for messages in this group are POSTed here. "" = none.
	WebhookURL string
}

// GroupConversation is one group entry of the conversation list (S14).
// Unread counts messages from other members beyond the viewer's cursor
// (system messages, e.g. the S17 creation event, are excluded). Muted and
// Ignored are the viewer's own prefs for the group (S17).
type GroupConversation struct {
	GroupID     int
	Name        string
	Kind        string // S21: "group" | "channel"
	MemberCount int
	LastMsgID   int // 0 when the group has no messages yet
	LastMsgAt   string
	LastMsgKind string
	LastMsgBody string
	Unread      int
	Muted       bool
	Ignored     bool
}

// groupPeerID maps a group id to the conversation-list peer convention:
// peer_id = -(group_id+1). It is also the conv_reads key used for the
// group's read cursor.
func groupPeerID(groupID int) int { return -(groupID + 1) }

// CreateGroup creates a group (or project channel, S21) and its members in
// one transaction. The name is trimmed and must be 1-64 runes; members are
// de-duplicated and always include the creator; the result must have at
// least 2 members. Member 0 is the boss (legal, has no employees row); any
// other member must exist. kind is "group" (default, any caller) or
// "channel" (boss-only); an empty kind is treated as "group".
func (s *Store) CreateGroup(name string, creatorID int, memberIDs []int, kind string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return 0, fmt.Errorf("store: create group: name must be 1-64 characters")
	}
	if kind == "" {
		kind = "group"
	}
	switch kind {
	case "group", "channel":
	default:
		return 0, fmt.Errorf("store: create group: invalid kind %q (want group or channel)", kind)
	}
	if kind == "channel" && creatorID != 0 {
		return 0, fmt.Errorf("store: create group: channel: only boss may create")
	}
	seen := map[int]bool{creatorID: true}
	members := []int{creatorID}
	for _, id := range memberIDs {
		if !seen[id] {
			seen[id] = true
			members = append(members, id)
		}
	}
	if len(members) < 2 {
		return 0, fmt.Errorf("store: create group: need at least 2 members")
	}
	// S17: the creator's display name goes into the group-creation system
	// message. Resolve it before the transaction: the pool allows a single
	// connection, so no reads may run while a transaction holds it.
	creatorName := "boss"
	if creatorID > 0 {
		emp, err := s.GetEmployee(creatorID)
		if err != nil {
			return 0, fmt.Errorf("store: create group: creator %d: %w", creatorID, err)
		}
		creatorName = emp.Name
	}
	for _, id := range members {
		if id > 0 && id != creatorID {
			if _, err := s.GetEmployee(id); err != nil {
				return 0, fmt.Errorf("store: create group: member %d: %w", id, err)
			}
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: create group: %w", err)
	}
	res, err := tx.Exec(
		`INSERT INTO groups (name, created_by, kind) VALUES (?, ?, ?)`, name, creatorID, kind)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("store: create group: %w", err)
	}
	gid, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("store: create group: %w", err)
	}
	for _, id := range members {
		if _, err := tx.Exec(`INSERT INTO group_members (group_id, employee_id) VALUES (?, ?)`, gid, id); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("store: create group: add member %d: %w", id, err)
		}
	}
	// S17: the creation event is a system message of the group, so brand-new
	// groups carry a last message and sort to the top of the conversation
	// list (Q1 fix). from_id stays NULL for the boss (creator 0).
	var fromID *int
	if creatorID > 0 {
		creator := creatorID
		fromID = &creator
	}
	kindLabel := "group"
	if kind == "channel" {
		kindLabel = "project channel"
	}
	if _, err := tx.Exec(`
		INSERT INTO messages (from_id, to_id, kind, body, group_id)
		VALUES (?, NULL, ?, ?, ?)`, fromID, MsgSystem, creatorName+" created a "+kindLabel, gid); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("store: create group: insert system message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: create group: %w", err)
	}
	return int(gid), nil
}

// GetGroup returns a group by id.
func (s *Store) GetGroup(id int) (Group, error) {
	var g Group
	var webhook sql.NullString
	err := s.db.QueryRow(`
		SELECT id, name, created_by, created_at, kind,
		       default_priority, sla_timeout_seconds, webhook_url
		FROM groups WHERE id = ?`, id).
		Scan(&g.ID, &g.Name, &g.CreatedBy, &g.CreatedAt, &g.Kind,
			&g.DefaultPriority, &g.SlaTimeoutSeconds, &webhook)
	if err != nil {
		if err == sql.ErrNoRows {
			return g, ErrNotFound
		}
		return g, fmt.Errorf("store: get group %d: %w", id, err)
	}
	g.WebhookURL = webhook.String
	return g, nil
}

// ListGroups returns all groups with their S30 policy columns, ordered by id.
// Intentionally light (no name/created_at) for scheduler sweeps.
func (s *Store) ListGroups() ([]Group, error) {
	rows, err := s.db.Query(
		`SELECT id, default_priority, sla_timeout_seconds FROM groups ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list groups: %w", err)
	}
	defer rows.Close()
	out := make([]Group, 0, 8)
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.DefaultPriority, &g.SlaTimeoutSeconds); err != nil {
			return nil, fmt.Errorf("store: list groups: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list groups: %w", err)
	}
	return out, nil
}

// SetGroupPolicy partially updates a group's S30 policy: defaultPriority (0..99)
// and/or slaTimeoutSeconds (0..2592000, 0 = disabled). A nil pointer leaves
// that field unchanged (COALESCE). An unknown group is ErrNotFound.
func (s *Store) SetGroupPolicy(ctx context.Context, groupID int, defaultPriority, slaTimeout *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE groups
		SET default_priority = COALESCE(?, default_priority),
		    sla_timeout_seconds = COALESCE(?, sla_timeout_seconds)
		WHERE id = ?`, defaultPriority, slaTimeout, groupID)
	if err != nil {
		return fmt.Errorf("store: set group policy %d: %w", groupID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: set group policy %d rows affected: %w", groupID, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetGroupWebhook (V-4) sets the group's webhook URL. nil clears the value
// (no webhook). Unknown group is ErrNotFound.
func (s *Store) SetGroupWebhook(ctx context.Context, groupID int, webhookURL *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var urlArg any
	if webhookURL != nil {
		urlArg = *webhookURL
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE groups SET webhook_url = ? WHERE id = ?`, urlArg, groupID)
	if err != nil {
		return fmt.Errorf("store: set group webhook %d: %w", groupID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: set group webhook %d rows affected: %w", groupID, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// IsGroupMember reports whether empID (0 = boss) belongs to the group.
func (s *Store) IsGroupMember(groupID, empID int) (bool, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM group_members WHERE group_id = ? AND employee_id = ?`,
		groupID, empID).Scan(&n); err != nil {
		return false, fmt.Errorf("store: is group member: %w", err)
	}
	return n > 0, nil
}

// GroupMemberIDs returns the member employee ids of a group (0 = boss).
func (s *Store) GroupMemberIDs(groupID int) ([]int, error) {
	rows, err := s.db.Query(
		`SELECT employee_id FROM group_members WHERE group_id = ? ORDER BY employee_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("store: group members: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: group members: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GroupReadCursors returns all members' read cursors for a group:
// viewer_id → last_read_msg_id.  Used to compute per-message read receipts
// (S19-B).  The boss is viewer 0.
func (s *Store) GroupReadCursors(groupID int) (map[int]int, error) {
	out := map[int]int{}
	rows, err := s.db.Query(
		`SELECT viewer_id, last_read_msg_id FROM conv_reads WHERE peer_id = ?`, groupPeerID(groupID))
	if err != nil {
		return nil, fmt.Errorf("store: group read cursors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var viewer, msgID int
		if err := rows.Scan(&viewer, &msgID); err != nil {
			return nil, fmt.Errorf("store: group read cursors: %w", err)
		}
		out[viewer] = msgID
	}
	return out, rows.Err()
}

// ListGroupConversations returns the groups viewerID (0 = boss) belongs to,
// most recent activity first, each with its last message and unread count
// (read cursor shared with 1:1 conversations via peer_id = -(group_id+1)).
func (s *Store) ListGroupConversations(viewerID int) ([]GroupConversation, error) {
	rows, err := s.db.Query(`
		SELECT g.id, g.name, g.kind,
		       (SELECT COUNT(*) FROM group_members gm WHERE gm.group_id = g.id) AS member_count,
		       COALESCE(lm.max_id, 0)    AS last_msg_id,
		       COALESCE(m.created_at, '') AS last_at,
		       COALESCE(m.kind, '')       AS last_kind,
		       COALESCE(substr(m.body, 1, 160), '') AS last_body,
		       COALESCE(gp.muted, 0)   AS muted,
		       COALESCE(gp.ignored, 0) AS ignored
		FROM groups g
		LEFT JOIN (SELECT group_id, MAX(id) AS max_id
		           FROM messages WHERE group_id IS NOT NULL
		           GROUP BY group_id) lm ON lm.group_id = g.id
		LEFT JOIN messages m ON m.id = lm.max_id
		LEFT JOIN group_prefs gp ON gp.group_id = g.id AND gp.viewer_id = ?
		WHERE EXISTS (SELECT 1 FROM group_members gm
		              WHERE gm.group_id = g.id AND gm.employee_id = ?)
		ORDER BY COALESCE(lm.max_id, 0) DESC, g.id ASC`, viewerID, viewerID)
	if err != nil {
		return nil, fmt.Errorf("store: list group conversations: %w", err)
	}
	defer rows.Close()

	out := make([]GroupConversation, 0, 8)
	for rows.Next() {
		var gc GroupConversation
		var muted, ignored int
		if err := rows.Scan(&gc.GroupID, &gc.Name, &gc.Kind, &gc.MemberCount,
			&gc.LastMsgID, &gc.LastMsgAt, &gc.LastMsgKind, &gc.LastMsgBody,
			&muted, &ignored); err != nil {
			return nil, fmt.Errorf("store: list group conversations: %w", err)
		}
		gc.Muted, gc.Ignored = muted == 1, ignored == 1
		out = append(out, gc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list group conversations: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	// Unread: messages from other members beyond this viewer's cursor.
	cursors, err := s.ReadCursors(viewerID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		var n int
		// System messages (S17 group-creation event) never count as unread.
		if err := s.db.QueryRow(`
			SELECT COUNT(*) FROM messages
			WHERE group_id = ? AND kind <> 'system' AND COALESCE(from_id, 0) <> ? AND id > ?`,
			out[i].GroupID, viewerID, cursors[groupPeerID(out[i].GroupID)]).Scan(&n); err != nil {
			return nil, fmt.Errorf("store: list group conversations: unread %d: %w", out[i].GroupID, err)
		}
		out[i].Unread = n
	}
	return out, nil
}

// GroupMessages returns the group's latest messages (at most limit, default
// 50), oldest first.
func (s *Store) GroupMessages(groupID, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, from_id, to_id, kind, body, project_id, task_id, reply_to, group_id, created_at FROM (
			SELECT id, from_id, to_id, kind, body, project_id, task_id, reply_to, group_id, created_at
			FROM messages WHERE group_id = ?
			ORDER BY id DESC
			LIMIT ?
		) ORDER BY id ASC`, groupID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: group messages %d: %w", groupID, err)
	}
	return scanMessages(rows, fmt.Sprintf("store: group messages %d", groupID))
}

// CreateGroupMessage inserts a group message (S14): group_id set, to_id NULL.
// kind (S22) may be any message kind allowed by the messages CHECK constraint
// ("chat" for plain group chat, "dispatch" for S22 group task dispatch,
// "report" for S22 group finish reports, ...); the DB enforces the set.
// fromID nil means the boss. replyTo (S16, optional) is the id of the
// replied-to group message, stored as-is.
func (s *Store) CreateGroupMessage(groupID int, kind, body string, fromID *int, fileIDs []int, replyTo *int) (int, error) {
	if _, err := s.GetGroup(groupID); err != nil {
		return 0, fmt.Errorf("store: create group message: %w", err)
	}
	if fromID != nil {
		if _, err := s.GetEmployee(*fromID); err != nil {
			return 0, fmt.Errorf("store: create group message: sender: %w", err)
		}
	}
	// S20: real mentions — participants are all group members (boss id 0
	// included when a member); the sender never mentions themselves.
	var mids []int
	if members, err := s.GroupMemberIDs(groupID); err != nil {
		return 0, fmt.Errorf("store: create group message: members: %w", err)
	} else {
		sender := 0
		if fromID != nil {
			sender = *fromID
		}
		if mids, err = s.MentionIDsIn(body, members, sender); err != nil {
			return 0, err
		}
	}
	res, err := s.db.Exec(`
		INSERT INTO messages (from_id, to_id, kind, body, group_id, reply_to)
		VALUES (?, NULL, ?, ?, ?, ?)`, fromID, kind, body, groupID, replyTo)
	if err != nil {
		return 0, fmt.Errorf("store: create group message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create group message: %w", err)
	}
	if len(mids) > 0 {
		if err := s.AddMessageMentions(int(id), mids); err != nil {
			return 0, err
		}
	}
	if len(fileIDs) > 0 {
		if err := s.LinkFilesToMessage(int(id), fileIDs); err != nil {
			return 0, err
		}
	}
	return int(id), nil
}

// LatestGroupMessageID returns the newest message id of the group
// (0 when it has none).
func (s *Store) LatestGroupMessageID(groupID int) (int, error) {
	var id int
	err := s.db.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM messages WHERE group_id = ?`, groupID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: latest group message: %w", err)
	}
	return id, nil
}

// GroupMemberDetail is one member of a group with identity info (S17 group
// settings). EmployeeID 0 is the boss.
type GroupMemberDetail struct {
	EmployeeID int
	Name       string
	Kind       string // "system" for the boss, else "ai"/"human"
	Role       string
	IsCreator  bool
}

// ListGroupMemberDetails returns the group's members (boss row when present,
// then employees), ascending by employee_id, with kind/role and the creator
// flag. A missing group is ErrNotFound.
func (s *Store) ListGroupMemberDetails(groupID int) ([]GroupMemberDetail, error) {
	g, err := s.GetGroup(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT gm.employee_id, COALESCE(e.name, ''), COALESCE(e.kind, ''), COALESCE(e.role, '')
		FROM group_members gm
		LEFT JOIN employees e ON e.id = gm.employee_id
		WHERE gm.group_id = ?
		ORDER BY gm.employee_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("store: group member details %d: %w", groupID, err)
	}
	defer rows.Close()

	out := make([]GroupMemberDetail, 0, 8)
	for rows.Next() {
		var d GroupMemberDetail
		if err := rows.Scan(&d.EmployeeID, &d.Name, &d.Kind, &d.Role); err != nil {
			return nil, fmt.Errorf("store: group member details %d: %w", groupID, err)
		}
		if d.EmployeeID == 0 {
			d.Name, d.Kind, d.Role = "boss", "system", ""
		} else if d.Name == "" {
			continue // stale row for a deleted employee
		}
		d.IsCreator = d.EmployeeID == g.CreatedBy
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: group member details %d: %w", groupID, err)
	}
	return out, nil
}

// GetGroupPrefs returns the viewer's (0 = boss) mute/ignore prefs for a
// group; missing rows read as (false, false). A missing group is ErrNotFound.
func (s *Store) GetGroupPrefs(viewerID, groupID int) (bool, bool, error) {
	if _, err := s.GetGroup(groupID); err != nil {
		return false, false, err
	}
	var muted, ignored int
	err := s.db.QueryRow(`
		SELECT COALESCE((SELECT muted FROM group_prefs WHERE group_id = ? AND viewer_id = ?), 0),
		       COALESCE((SELECT ignored FROM group_prefs WHERE group_id = ? AND viewer_id = ?), 0)`,
		groupID, viewerID, groupID, viewerID).Scan(&muted, &ignored)
	if err != nil {
		return false, false, fmt.Errorf("store: group prefs %d/%d: %w", groupID, viewerID, err)
	}
	return muted == 1, ignored == 1, nil
}

// SetGroupPrefs upserts the viewer's (0 = boss) mute/ignore prefs for a
// group; a nil pointer leaves that flag unchanged. A missing group is
// ErrNotFound.
func (s *Store) SetGroupPrefs(viewerID, groupID int, muted, ignored *bool) error {
	if _, err := s.GetGroup(groupID); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		INSERT INTO group_prefs (group_id, viewer_id, muted, ignored)
		VALUES (?, ?, COALESCE(?, 0), COALESCE(?, 0))
		ON CONFLICT (group_id, viewer_id)
		DO UPDATE SET muted = COALESCE(?, group_prefs.muted),
		              ignored = COALESCE(?, group_prefs.ignored)`,
		groupID, viewerID, muted, ignored, muted, ignored)
	if err != nil {
		return fmt.Errorf("store: set group prefs %d/%d: %w", groupID, viewerID, err)
	}
	return nil
}

// AddGroupMembers adds employeeIDs (0 = boss, or existing employees) to an
// existing group, skipping duplicates and already-present members. Returns
// the ids actually added, in input order. An all-duplicate or empty list is
// an error. All reads run before the transaction (single-connection pool).
func (s *Store) AddGroupMembers(groupID int, employeeIDs []int) ([]int, error) {
	if _, err := s.GetGroup(groupID); err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	ids := make([]int, 0, len(employeeIDs))
	for _, id := range employeeIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if id > 0 {
			if _, err := s.GetEmployee(id); err != nil {
				return nil, fmt.Errorf("store: add group members: member %d: %w", id, err)
			}
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("store: add group members: %w", ErrNoNewGroupMembers)
	}
	rows, err := s.db.Query(
		`SELECT employee_id FROM group_members WHERE group_id = ?`, groupID)
	if err != nil {
		return nil, fmt.Errorf("store: add group members: %w", err)
	}
	existing := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: add group members: %w", err)
		}
		existing[id] = true
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: add group members: %w", err)
	}
	added := make([]int, 0, len(ids))
	for _, id := range ids {
		if !existing[id] {
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return nil, fmt.Errorf("store: add group members: %w", ErrNoNewGroupMembers)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: add group members: %w", err)
	}
	for _, id := range added {
		if _, err := tx.Exec(
			`INSERT INTO group_members (group_id, employee_id) VALUES (?, ?)`, groupID, id); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("store: add group members: add %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: add group members: %w", err)
	}
	return added, nil
}

// RenameGroup renames a group (S26). The name is trimmed and must be 1-64
// runes; an unknown group is ErrNotFound.
func (s *Store) RenameGroup(ctx context.Context, groupID int, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("store: rename group: name must be 1-64 characters")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE groups SET name = ? WHERE id = ?`, name, groupID)
	if err != nil {
		return fmt.Errorf("store: rename group %d: %w", groupID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: rename group %d: %w", groupID, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RemoveGroupMember removes employeeID from a group (S26). The boss
// (employee_id 0) cannot be removed; removing a non-member is an error; a
// group must keep at least 2 members after the removal (same invariant as
// CreateGroup). Since S31, channels (kind=channel) may shrink to a single
// remaining member — the boss row is protected above, so a channel can
// never become empty — while groups keep the >=2 floor. An unknown group is
// ErrNotFound. The count + delete runs in one transaction so concurrent
// removals of different members cannot each see total>=3 and both delete,
// leaving the group with 1 member.
func (s *Store) RemoveGroupMember(ctx context.Context, groupID, employeeID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if employeeID == 0 {
		return fmt.Errorf("store: remove group member: the boss cannot be removed")
	}
	g, err := s.GetGroup(groupID)
	if err != nil {
		return err
	}
	minLeft := 2
	if g.Kind == "channel" {
		minLeft = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: remove group member: %w", err)
	}
	var inGroup int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM group_members WHERE group_id = ? AND employee_id = ?`,
		groupID, employeeID).Scan(&inGroup); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: remove group member: %w", err)
	}
	if inGroup == 0 {
		_ = tx.Rollback()
		return fmt.Errorf("store: remove group member: employee %d is not in group %d", employeeID, groupID)
	}
	var total int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM group_members WHERE group_id = ?`, groupID).Scan(&total); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: remove group member: %w", err)
	}
	if total-1 < minLeft {
		_ = tx.Rollback()
		return fmt.Errorf("store: remove group member: group %d would have fewer than %d members", groupID, minLeft)
	}
	if _, err := tx.Exec(
		`DELETE FROM group_members WHERE group_id = ? AND employee_id = ?`, groupID, employeeID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: remove group member: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: remove group member: %w", err)
	}
	return nil
}

// ErrGroupHasOpenTasks is returned by DeleteGroup when the group still has
// open (queued/running) tasks; it carries the count so the API layer can
// build the 409 response body.
type ErrGroupHasOpenTasks struct {
	Count int
}

func (e ErrGroupHasOpenTasks) Error() string {
	return fmt.Sprintf("store: delete group: %d open task(s) remain", e.Count)
}

// DeleteGroup deletes a group (S26), in one transaction. The FK ON DELETE
// CASCADE removes group_members, group_prefs and the group's messages;
// tasks.group_id is ON DELETE SET NULL, so task rows survive with
// group_id NULL (task history is preserved). conv_reads carries no FK
// (peer_id = -(group_id+1)), so the group's read cursors are cleared
// explicitly. An unknown group is ErrNotFound. Open-task count is re-checked
// inside the transaction (authoritative guard against the TOCTOU window of
// any API-layer pre-check); a positive count aborts with
// ErrGroupHasOpenTasks.
func (s *Store) DeleteGroup(ctx context.Context, groupID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.GetGroup(groupID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete group %d: %w", groupID, err)
	}
	var open int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE group_id = ? AND status IN ('queued', 'running')`,
		groupID).Scan(&open); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: delete group %d: %w", groupID, err)
	}
	if open > 0 {
		_ = tx.Rollback()
		return ErrGroupHasOpenTasks{Count: open}
	}
	if _, err := tx.Exec(
		`DELETE FROM conv_reads WHERE peer_id = ?`, groupPeerID(groupID)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: delete group %d: clear read cursors: %w", groupID, err)
	}
	if _, err := tx.Exec(`DELETE FROM groups WHERE id = ?`, groupID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: delete group %d: %w", groupID, err)
	}
	// S31: on legacy (pre-S31) databases, departments.group_id has no FK
	// (SQLite ALTER cannot add one). Clear it manually after deleting a
	// group, matching the ON DELETE SET NULL behavior of fresh databases.
	if _, err := tx.Exec(
		`UPDATE departments SET group_id = NULL WHERE group_id = ?`, groupID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: delete group %d: clear departments.group_id: %w", groupID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete group %d: %w", groupID, err)
	}
	return nil
}
