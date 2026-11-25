package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Message kinds.
const (
	MsgReport     = "report"
	MsgDispatch   = "dispatch"
	MsgAsk        = "ask"
	MsgAnswer     = "answer"
	MsgChat       = "chat"
	MsgSummary    = "summary"
	MsgSystem     = "system"
	MsgScheduled  = "scheduled"
	MsgSelfReview = "selfreview"
	// MsgReview (leader orchestration, S1) is the message half of the review
	// wake: Flow.Send(0, requester, KindReview, ...) lands it in the
	// requester's boss thread with from_id NULL. Same literal as
	// KindReview — both CHECK constraints and this whitelist carry it.
	MsgReview = "review"
)

// Message is a routed team communication.
type Message struct {
	ID        int
	FromID    *int // nil = system/boss
	ToID      *int // nil = group message (group_id set) or system message
	Kind      string
	Body      string
	ProjectID *int
	TaskID    *int
	ReplyTo   *int // S16: id of the replied-to message (no FK; dangling refs tolerated)
	GroupID   *int // S19: set for group messages (to_id is NULL)
	CreatedAt string
}

// CreateMessage inserts a message. fromID nil means the boss/system.
// replyTo (S16, optional) is the id of the replied-to message; it is stored
// as-is without an existence check — validation happens at the API layer
// (SameConversation) because the client only sends it on explicit "reply".
func (s *Store) CreateMessage(toID int, kind, body string, fromID, projectID, taskID, replyTo *int) (int, error) {
	if _, err := s.GetEmployee(toID); err != nil {
		return 0, fmt.Errorf("store: create message: recipient: %w", err)
	}
	if fromID != nil {
		if _, err := s.GetEmployee(*fromID); err != nil {
			return 0, fmt.Errorf("store: create message: sender: %w", err)
		}
	}
	if projectID != nil {
		if _, err := s.GetProject(*projectID); err != nil {
			return 0, fmt.Errorf("store: create message: project: %w", err)
		}
	}
	if taskID != nil {
		if _, err := s.GetTask(*taskID); err != nil {
			return 0, fmt.Errorf("store: create message: task: %w", err)
		}
	}
	valid := map[string]bool{
		MsgReport: true, MsgDispatch: true, MsgAsk: true, MsgAnswer: true,
		MsgChat: true, MsgSummary: true, MsgSystem: true,
		MsgScheduled: true, MsgSelfReview: true, MsgReview: true,
	}
	if !valid[kind] {
		return 0, fmt.Errorf("store: create message: unsupported kind %s", kind)
	}
	// S20: real mentions — extract targets before the insert (participants
	// are the recipient plus the sender; the boss has id 0). System
	// messages carry no mentions.
	sender := 0
	if fromID != nil {
		sender = *fromID
	}
	parts := []int{toID}
	if sender != 0 {
		parts = append(parts, sender)
	}
	var mids []int
	if kind != MsgSystem {
		var err error
		if mids, err = s.MentionIDsIn(body, parts, sender); err != nil {
			return 0, err
		}
	}
	res, err := s.db.Exec(`
		INSERT INTO messages (from_id, to_id, kind, body, project_id, task_id, reply_to)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, fromID, toID, kind, body, projectID, taskID, replyTo)
	if err != nil {
		return 0, fmt.Errorf("store: create message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create message: %w", err)
	}
	if len(mids) > 0 {
		if err := s.AddMessageMentions(int(id), mids); err != nil {
			return 0, err
		}
	}
	return int(id), nil
}

// SetMessageTaskID links a message to the task created for it (back-fill).
func (s *Store) SetMessageTaskID(messageID, taskID int) error {
	res, err := s.db.Exec(
		`UPDATE messages SET task_id = ? WHERE id = ?`, taskID, messageID)
	if err != nil {
		return fmt.Errorf("store: set message task id: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("store: set message task id: message %d not found", messageID)
	}
	return nil
}

// DispatchMessageIDForGroupTask returns the id of the dispatch message of
// group groupID whose task_id points at taskID — the S24 group-task board's
// jump anchor (row click scrolls back to the dispatch bubble). It returns
// (0, nil) when no such message exists (the API layer renders that as
// message_id=null, making the row non-clickable).
func (s *Store) DispatchMessageIDForGroupTask(groupID, taskID int) (int, error) {
	var id int
	err := s.db.QueryRow(
		`SELECT id FROM messages WHERE group_id = ? AND task_id = ? AND kind = ?
		 ORDER BY id LIMIT 1`, groupID, taskID, MsgDispatch).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: dispatch message of group %d task %d: %w", groupID, taskID, err)
	}
	return id, nil
}

// GetMessage returns a message by id.
func (s *Store) GetMessage(id int) (Message, error) {
	var m Message
	var from, to, proj, task, reply, grp sql.NullInt64
	err := s.db.QueryRow(`
		SELECT id, from_id, to_id, kind, body, project_id, task_id, reply_to, group_id, created_at
		FROM messages WHERE id = ?`, id).
		Scan(&m.ID, &from, &to, &m.Kind, &m.Body, &proj, &task, &reply, &grp, &m.CreatedAt)
	if err != nil {
		return m, fmt.Errorf("store: get message %d: %w", id, err)
	}
	m.FromID = nullInt64(from)
	m.ToID = nullInt64(to)
	m.ProjectID = nullInt64(proj)
	m.TaskID = nullInt64(task)
	m.ReplyTo = nullInt64(reply)
	m.GroupID = nullInt64(grp)
	return m, nil
}

// SameConversation reports whether message replyTo lives in the same
// conversation as a new message from `from` (S16). toID != nil means a 1:1
// conversation with employee *toID: the target's (COALESCE(from_id,0), to_id)
// pair must match (from, *toID) in either direction. group != nil means a
// group conversation: the target must be a message of group *group
// (group_id set, to_id NULL). A missing target returns (false, nil) so the
// API layer answers 400 uniformly.
func (s *Store) SameConversation(replyTo, from int, toID *int, group *int) (bool, error) {
	target, err := s.GetMessage(replyTo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: same conversation: %w", err)
	}
	if group != nil {
		var gid int
		if err := s.db.QueryRow(
			`SELECT COALESCE(group_id, 0) FROM messages WHERE id = ?`, replyTo).Scan(&gid); err != nil {
			return false, fmt.Errorf("store: same conversation: %w", err)
		}
		return gid == *group && target.ToID == nil, nil
	}
	if toID == nil {
		return false, fmt.Errorf("store: same conversation: neither to_id nor group given")
	}
	if target.ToID == nil {
		return false, nil // group/system message: not part of any 1:1 thread
	}
	tf := 0
	if target.FromID != nil {
		tf = *target.FromID
	}
	return (tf == from && *target.ToID == *toID) || (tf == *toID && *target.ToID == from), nil
}

// ListMessagesTo returns messages addressed to one employee, newest first.
func (s *Store) ListMessagesTo(empID int, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(`
		SELECT id, from_id, to_id, kind, body, project_id, task_id, reply_to, group_id, created_at
		FROM messages WHERE to_id = ? ORDER BY id DESC LIMIT ?`, empID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list messages to employee %d: %w", empID, err)
	}
	return scanMessages(rows, fmt.Sprintf("store: list messages to employee %d", empID))
}

// ListMessagesByProject returns all messages of a project, oldest first.
func (s *Store) ListMessagesByProject(projectID int) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, from_id, to_id, kind, body, project_id, task_id, reply_to, group_id, created_at
		FROM messages WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list messages of project %d: %w", projectID, err)
	}
	return scanMessages(rows, fmt.Sprintf("store: list messages of project %d", projectID))
}

func scanMessages(rows *sql.Rows, ctx string) ([]Message, error) {
	defer rows.Close()
	out := make([]Message, 0, 32)
	for rows.Next() {
		var m Message
		var from, to, proj, task, reply, grp sql.NullInt64
		if err := rows.Scan(&m.ID, &from, &to, &m.Kind, &m.Body, &proj, &task, &reply, &grp, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("%s: %w", ctx, err)
		}
		m.FromID = nullInt64(from)
		m.ToID = nullInt64(to)
		m.ProjectID = nullInt64(proj)
		m.TaskID = nullInt64(task)
		m.ReplyTo = nullInt64(reply)
		m.GroupID = nullInt64(grp)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", ctx, err)
	}
	return out, nil
}

// DispatchMessageIDsForGroupTasks maps group-task ids to their dispatch
// message ids in chunked batch queries (F-2 pattern). For each task it
// returns the oldest dispatch message in the group (MIN(id)), matching the
// singular DispatchMessageIDForGroupTask contract; tasks without a dispatch
// message are absent from the result map.
func (s *Store) DispatchMessageIDsForGroupTasks(groupID int, taskIDs []int) (map[int]int, error) {
	out := make(map[int]int, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	const batchSize = 500
	for start := 0; start < len(taskIDs); start += batchSize {
		end := start + batchSize
		if end > len(taskIDs) {
			end = len(taskIDs)
		}
		chunk := taskIDs[start:end]
		args := []any{groupID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(
			"SELECT task_id, MIN(id) FROM messages WHERE group_id = ? AND kind = 'dispatch' AND task_id IN ("+inPlaceholders(len(chunk))+") GROUP BY task_id",
			args...)
		if err != nil {
			return nil, fmt.Errorf("store: dispatch message ids for group %d: %w", groupID, err)
		}
		for rows.Next() {
			var taskID int
			var msgID int
			if err := rows.Scan(&taskID, &msgID); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: dispatch message ids scan: %w", err)
			}
			out[taskID] = msgID
		}
		scanErr := rows.Err()
		rows.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("store: dispatch message ids rows: %w", scanErr)
		}
	}
	return out, nil
}
