package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Conversation is one entry of the caller's conversation list (chat view,
// S8). PeerID 0 means the boss/system side (messages with from_id NULL).
type Conversation struct {
	PeerID      int
	LastMsgID   int // 0 when only an open task links the two
	LastMsgAt   string
	LastMsgKind string
	LastMsgBody string // truncated preview
	Unread      int    // messages to the viewer from this peer beyond the read cursor
	OpenTasks   int    // open (queued/running) tasks between the viewer and this peer
}

// ListConversations returns the conversation list for viewerID (0 = boss),
// most recent activity first. A peer appears when at least one message
// exists in either direction, or an open task links the two.
func (s *Store) ListConversations(viewerID int) ([]Conversation, error) {
	peers, err := s.conversationPeers(viewerID)
	if err != nil {
		return nil, err
	}
	if len(peers) == 0 {
		return []Conversation{}, nil
	}
	cursors, err := s.ReadCursors(viewerID)
	if err != nil {
		return nil, err
	}
	open, err := s.openTaskCounts(viewerID, peers)
	if err != nil {
		return nil, err
	}

	type lastMsg struct {
		id   int
		at   string
		kind string
		body string
	}
	perPeer := make(map[int]*lastMsg, len(peers))
	unread := make(map[int]int, len(peers))
	for _, p := range peers {
		perPeer[p] = &lastMsg{}
	}
	args := make([]any, 0, 2+len(peers)*2)
	args = append(args, viewerID)
	args = append(args, intsToAny(peers)...)
	args = append(args, viewerID)
	args = append(args, intsToAny(peers)...)
	rows, err := s.db.Query(`
		SELECT m.id, COALESCE(m.from_id, 0) AS f, m.to_id, m.kind,
		       substr(m.body, 1, 160) AS body, m.created_at
		FROM messages m
		WHERE (m.to_id = ? AND COALESCE(m.from_id, 0) IN (`+inPlaceholders(len(peers))+`))
		   OR (COALESCE(m.from_id, 0) = ? AND m.to_id IN (`+inPlaceholders(len(peers))+`))
		ORDER BY m.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list conversations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id        int
			from      int
			to        int
			kind      string
			body      string
			createdAt string
		)
		if err := rows.Scan(&id, &from, &to, &kind, &body, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list conversations: %w", err)
		}
		var peer int
		if to == viewerID {
			peer = from // incoming: the peer is who sent it
		} else {
			peer = to // outgoing: the peer is the recipient
		}
		lm, ok := perPeer[peer]
		if !ok {
			continue
		}
		if id > lm.id {
			lm.id, lm.at, lm.kind, lm.body = id, createdAt, kind, body
		}
		if to == viewerID && from == peer && id > cursors[peer] {
			unread[peer]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list conversations: %w", err)
	}

	out := make([]Conversation, 0, len(peers))
	for _, p := range peers {
		lm := perPeer[p]
		out = append(out, Conversation{
			PeerID:      p,
			LastMsgID:   lm.id,
			LastMsgAt:   lm.at,
			LastMsgKind: lm.kind,
			LastMsgBody: lm.body,
			Unread:      unread[p],
			OpenTasks:   open[p],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastMsgID != out[j].LastMsgID {
			return out[i].LastMsgID > out[j].LastMsgID
		}
		return out[i].PeerID < out[j].PeerID
	})
	return out, nil
}

// conversationPeers returns the set of peer IDs that have a conversation
// with viewerID: message counterparts in either direction, plus the other
// side of any open task.
func (s *Store) conversationPeers(viewerID int) ([]int, error) {
	// Task-derived peers: an open task links requester and worker, so both
	// sides appear (boss can only be the requester).
	var taskPart string
	if viewerID == 0 {
		taskPart = `SELECT employee_id AS peer FROM tasks
			WHERE requested_by = 0 AND status IN ('queued', 'running')`
	} else {
		taskPart = `SELECT requested_by AS peer FROM tasks
			WHERE employee_id = ? AND status IN ('queued', 'running')
			UNION
			SELECT employee_id AS peer FROM tasks
			WHERE requested_by = ? AND status IN ('queued', 'running')`
	}
	q := `
		SELECT peer FROM (
		SELECT to_id AS peer FROM messages
			WHERE COALESCE(from_id, 0) = ? AND to_id IS NOT NULL AND to_id <> ?
			UNION
			SELECT COALESCE(from_id, 0) AS peer FROM messages
				WHERE to_id = ? AND COALESCE(from_id, 0) <> ?
			UNION
			` + taskPart + `
		) ORDER BY peer`
	args := []any{viewerID, viewerID, viewerID, viewerID}
	if viewerID != 0 {
		args = append(args, viewerID, viewerID)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: conversation peers: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: conversation peers: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// openTaskCounts returns, per peer, how many open tasks link viewerID to
// that peer (boss view: tasks the boss requested; employee view: tasks the
// viewer runs, keyed by requester).
func (s *Store) openTaskCounts(viewerID int, peers []int) (map[int]int, error) {
	out := make(map[int]int, len(peers))
	var q string
	var args []any
	if viewerID == 0 {
		// Boss only requests: peers are the workers of its open tasks.
		q = `SELECT employee_id AS peer, COUNT(*) FROM tasks
			WHERE requested_by = 0 AND employee_id IN (` + inPlaceholders(len(peers)) + `)
			  AND status IN ('queued', 'running') GROUP BY employee_id`
		args = intsToAny(peers)
	} else {
		// Employee: open tasks it executes (peer = requester) plus open
		// tasks it requested (peer = worker).
		ph := inPlaceholders(len(peers))
		q = `SELECT peer, COUNT(*) FROM (
			SELECT CASE WHEN employee_id = ? THEN requested_by ELSE employee_id END AS peer
			FROM tasks
			WHERE status IN ('queued', 'running')
			  AND ((employee_id = ? AND requested_by IN (` + ph + `))
			    OR (requested_by = ? AND employee_id IN (` + ph + `)))
		) GROUP BY peer`
		args = append([]any{viewerID, viewerID}, intsToAny(peers)...)
		args = append(args, viewerID)
		args = append(args, intsToAny(peers)...)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: open task counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p, n int
		if err := rows.Scan(&p, &n); err != nil {
			return nil, fmt.Errorf("store: open task counts: %w", err)
		}
		out[p] = n
	}
	return out, rows.Err()
}

// ReadCursors returns the viewer's read cursors as peer_id → last_read_msg_id.
// Group conversations use the negative peer-id convention (-(groupID+1)).
func (s *Store) ReadCursors(viewerID int) (map[int]int, error) {
	out := map[int]int{}
	rows, err := s.db.Query(`SELECT peer_id, last_read_msg_id FROM conv_reads WHERE viewer_id = ?`, viewerID)
	if err != nil {
		return nil, fmt.Errorf("store: read cursors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p, id int
		if err := rows.Scan(&p, &id); err != nil {
			return nil, fmt.Errorf("store: read cursors: %w", err)
		}
		out[p] = id
	}
	return out, rows.Err()
}

// ThreadMessages returns the bidirectional message thread between
// viewerID (0 = boss) and peerID, oldest first, at most limit messages
// (default 50).
func (s *Store) ThreadMessages(viewerID, peerID, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, from_id, to_id, kind, body, project_id, task_id, reply_to, group_id, created_at FROM (
			SELECT id, from_id, to_id, kind, body, project_id, task_id, reply_to, group_id, created_at
			FROM messages
			WHERE (to_id = ? AND COALESCE(from_id, 0) = ?)
			   OR (to_id = ? AND COALESCE(from_id, 0) = ?)
			ORDER BY id DESC
			LIMIT ?
		) ORDER BY id ASC`, viewerID, peerID, peerID, viewerID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: thread messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var from, proj, task, reply, grp sql.NullInt64
		var createdAt string
		if err := rows.Scan(&m.ID, &from, &m.ToID, &m.Kind, &m.Body, &proj, &task, &reply, &grp, &createdAt); err != nil {
			return nil, fmt.Errorf("store: thread messages: %w", err)
		}
		m.FromID = nullInt64(from)
		m.ProjectID = nullInt64(proj)
		m.TaskID = nullInt64(task)
		m.ReplyTo = nullInt64(reply)
		m.GroupID = nullInt64(grp)
		m.CreatedAt = createdAt
		out = append(out, m)
	}
	return out, rows.Err()
}

// LatestMessageIDTo returns the newest message ID from peerID to viewerID
// (0 when there is none). Used to advance the read cursor.
func (s *Store) LatestMessageIDTo(viewerID, peerID int) (int, error) {
	var id int
	err := s.db.QueryRow(`
		SELECT COALESCE(MAX(id), 0) FROM messages
		WHERE to_id = ? AND COALESCE(from_id, 0) = ?`, viewerID, peerID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: latest message: %w", err)
	}
	return id, nil
}

// MarkConversationRead advances the read cursor of (viewerID, peerID) to
// upToMsgID. Monotonic: a smaller upToMsgID never moves the cursor back.
// Returns true when the read state actually changed (new row inserted or the
// cursor advanced); false means no-op, so callers can skip emitting
// message.read events for unchanged state (S19-B loop fix: unconditional
// broadcasts fed the client-side refresh -> markRead -> refresh ping-pong).
func (s *Store) MarkConversationRead(viewerID, peerID, upToMsgID int) (bool, error) {
	res, err := s.db.Exec(`
		INSERT INTO conv_reads (viewer_id, peer_id, last_read_msg_id, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT (viewer_id, peer_id) DO UPDATE SET
			last_read_msg_id = MAX(conv_reads.last_read_msg_id, excluded.last_read_msg_id),
			updated_at = datetime('now')
		WHERE excluded.last_read_msg_id > conv_reads.last_read_msg_id`, viewerID, peerID, upToMsgID)
	if err != nil {
		return false, fmt.Errorf("store: mark read: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: mark read: rows affected: %w", err)
	}
	return n > 0, nil
}

func intsToAny(ids []int) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func inPlaceholders(n int) string {
	if n <= 0 {
		return "0"
	}
	b := make([]string, n)
	for i := range b {
		b[i] = "?"
	}
	return strings.Join(b, ",")
}
