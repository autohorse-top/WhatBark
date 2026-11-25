package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Task statuses.
const (
	TaskQueued   = "queued"
	TaskRunning  = "running"
	TaskDone     = "done"
	TaskFailed   = "failed"
	TaskCanceled = "canceled"
)

// Task kinds.
const (
	KindDispatch   = "dispatch"
	KindAsk        = "ask"
	KindScheduled  = "scheduled"
	KindSelfReview = "selfreview"
	// KindReview (leader orchestration, S1) is a system-injected review
	// wake: when a dispatch reaches done/failed, FinishTask creates one of
	// these for the AI requester so the leader reviews the result. It is
	// never user-dispatched (routes.Allowed pins from==0).
	KindReview = "review"
)

// Task is a unit of work assigned to one employee.
type Task struct {
	ID          int
	ProjectID   *int
	EmpID       int
	WorkflowID  *int
	GroupID     *int // S22: group the task was dispatched in (routes the finish report)
	RequestedBy int  // 0 = boss/user
	Kind        string
	Status      string
	Priority    int
	Input       string
	Output      string
	StartedAt   *string
	FinishedAt  *string
	CreatedAt   string
}

// allowedMoves is the task state machine: queued -> running|canceled;
// running -> done|failed|canceled; terminal states are immutable.
var allowedMoves = map[string]map[string]bool{
	TaskQueued:   {TaskRunning: true, TaskCanceled: true},
	TaskRunning:  {TaskDone: true, TaskFailed: true, TaskCanceled: true},
	TaskDone:     {},
	TaskFailed:   {},
	TaskCanceled: {},
}

// CreateTask inserts a queued task for an employee. priority must be in
// [0,99]; agents pick the highest-priority queued task first (tie: lowest id).
// S22: groupID (nullable) marks a group-dispatched task; it only routes the
// finish report into the group thread and feeds the conv-list open_tasks count.
func (s *Store) CreateTask(empID int, kind, input string, projectID, workflowID *int, requestedBy, priority int, groupID *int) (int, error) {
	if _, err := s.GetEmployee(empID); err != nil {
		return 0, fmt.Errorf("store: create task: employee: %w", err)
	}
	if kind != KindDispatch && kind != KindAsk && kind != KindScheduled && kind != KindSelfReview && kind != KindReview {
		return 0, fmt.Errorf("store: create task: unsupported kind %s", kind)
	}
	if priority < 0 || priority > 99 {
		return 0, fmt.Errorf("store: create task: priority %d out of range [0,99]", priority)
	}
	if projectID != nil {
		if _, err := s.GetProject(*projectID); err != nil {
			return 0, fmt.Errorf("store: create task: project: %w", err)
		}
	}
	if workflowID != nil {
		if _, err := s.GetWorkflow(*workflowID); err != nil {
			return 0, fmt.Errorf("store: create task: workflow: %w", err)
		}
	}
	if groupID != nil {
		if _, err := s.GetGroup(*groupID); err != nil {
			return 0, fmt.Errorf("store: create task: group: %w", err)
		}
	}
	res, err := s.db.Exec(`
		INSERT INTO tasks (project_id, employee_id, workflow_id, group_id, requested_by, kind, status, priority, input)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, empID, workflowID, groupID, requestedBy, kind, TaskQueued, priority, input)
	if err != nil {
		return 0, fmt.Errorf("store: create task for employee %d: %w", empID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create task: %w", err)
	}
	return int(id), nil
}

// GetTask returns a task by id.
func (s *Store) GetTask(id int) (Task, error) {
	var t Task
	var proj, wf, grp sql.NullInt64
	var started, finished sql.NullString
	err := s.db.QueryRow(`
		SELECT id, project_id, employee_id, workflow_id, group_id, requested_by, kind, status,
		       priority, input, output, started_at, finished_at, created_at
		FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &proj, &t.EmpID, &wf, &grp, &t.RequestedBy, &t.Kind, &t.Status,
			&t.Priority, &t.Input, &t.Output, &started, &finished, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("store: get task %d: %w", id, err)
	}
	t.ProjectID = nullInt64(proj)
	t.WorkflowID = nullInt64(wf)
	t.GroupID = nullInt64(grp)
	t.StartedAt = nullString(started)
	t.FinishedAt = nullString(finished)
	return t, nil
}

// ListTasksByEmployee returns tasks of one employee, optionally filtered by status.
func (s *Store) ListTasksByEmployee(empID int, status *string) ([]Task, error) {
	q := `SELECT id, project_id, employee_id, workflow_id, group_id, requested_by, kind, status,
	       priority, input, output, started_at, finished_at, created_at
	       FROM tasks WHERE employee_id = ?`
	args := []any{empID}
	if status != nil {
		q += ` AND status = ?`
		args = append(args, *status)
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks of employee %d: %w", empID, err)
	}
	return scanTasks(rows, fmt.Sprintf("store: list tasks of employee %d", empID))
}

// ListTasks returns all tasks, optionally filtered by status, newest first.
func (s *Store) ListTasks(status *string) ([]Task, error) {
	q := `SELECT id, project_id, employee_id, workflow_id, group_id, requested_by, kind, status,
	       priority, input, output, started_at, finished_at, created_at
	       FROM tasks`
	args := []any{}
	if status != nil {
		q += ` WHERE status = ?`
		args = append(args, *status)
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	return scanTasks(rows, "store: list tasks")
}

// ListTasksByProject returns all tasks of a project ordered by id.
func (s *Store) ListTasksByProject(projectID int) ([]Task, error) {
	rows, err := s.db.Query(`
		SELECT id, project_id, employee_id, workflow_id, group_id, requested_by, kind, status,
		       priority, input, output, started_at, finished_at, created_at
		FROM tasks WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks of project %d: %w", projectID, err)
	}
	return scanTasks(rows, fmt.Sprintf("store: list tasks of project %d", projectID))
}

// TaskStatuses (S28) returns a live status map for the given task ids. ids are
// queried in batches of <=500 and merged, so arbitrarily large id sets never
// hit SQLite's per-statement variable-count limit (F-2: an unbounded single IN
// list could exceed 32766 placeholders and 500 the request). Unknown ids are
// simply absent from the result; an empty (or nil) slice returns an empty map
// without issuing any SQL.
func (s *Store) TaskStatuses(ids []int) (map[int]string, error) {
	out := make(map[int]string, len(ids))
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		placeholders := make([]string, end-start)
		args := make([]any, end-start)
		for i := start; i < end; i++ {
			placeholders[i-start] = "?"
			args[i-start] = ids[i]
		}
		q := "SELECT id, status FROM tasks WHERE id IN (" + strings.Join(placeholders, ",") + ")"
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("store: task statuses for %d ids: %w", len(ids), err)
		}
		for rows.Next() {
			var id int
			var status string
			if err := rows.Scan(&id, &status); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: task statuses scan: %w", err)
			}
			out[id] = status
		}
		scanErr := rows.Err()
		rows.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("store: task statuses rows: %w", scanErr)
		}
	}
	return out, nil
}

// Tasks (task-duration-artifacts A) returns full Task rows for the given ids
// (same batching rule as TaskStatuses; unknown ids absent from the result).
func (s *Store) Tasks(ids []int) (map[int]Task, error) {
	out := make(map[int]Task, len(ids))
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		placeholders := make([]string, end-start)
		args := make([]any, end-start)
		for i := start; i < end; i++ {
			placeholders[i-start] = "?"
			args[i-start] = ids[i]
		}
		q := `SELECT id, project_id, employee_id, workflow_id, group_id, requested_by,
			kind, status, priority, input, output, started_at, finished_at, created_at
			FROM tasks WHERE id IN (` + strings.Join(placeholders, ",") + `)`
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("store: tasks for %d ids: %w", len(ids), err)
		}
		list, err := scanTasks(rows, fmt.Sprintf("store: tasks for %d ids", len(ids)))
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			out[t.ID] = t
		}
	}
	return out, nil
}

// ListGroupTasks returns a group's tasks (S24 board), newest first, at most
// limit rows. Tasks of other groups and 1:1 tasks (group_id NULL) never
// leak in; an unknown group id simply yields an empty slice (the API layer
// answers 404 via GetGroup).
//
// S29: projectID optionally filters the board by project — nil = all tasks
// (unchanged behavior), 0 = only tasks without a project (project_id IS
// NULL), >0 = only that project's tasks.
func (s *Store) ListGroupTasks(groupID, limit int, projectID *int) ([]Task, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `
		SELECT id, project_id, employee_id, workflow_id, group_id, requested_by, kind, status,
		       priority, input, output, started_at, finished_at, created_at
		FROM tasks WHERE group_id = ?`
	args := []any{groupID}
	if projectID != nil {
		if *projectID == 0 {
			q += ` AND project_id IS NULL`
		} else {
			q += ` AND project_id = ?`
			args = append(args, *projectID)
		}
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks of group %d: %w", groupID, err)
	}
	return scanTasks(rows, fmt.Sprintf("store: list tasks of group %d", groupID))
}

// ListOpenTasksSLADue (S30) returns the group's open tasks (queued/running)
// that have not yet received an SLA reminder and whose created_at is before
// cutoff. created_at is stored as 'YYYY-MM-DD HH:MM:SS' UTC (datetime('now')),
// so cutoff must be formatted in the same layout for a lexicographic compare.
func (s *Store) ListOpenTasksSLADue(groupID int, cutoff time.Time) ([]Task, error) {
	cutoffStr := cutoff.UTC().Format("2006-01-02 15:04:05")
	rows, err := s.db.Query(`
		SELECT id, project_id, employee_id, workflow_id, group_id, requested_by, kind, status,
		       priority, input, output, started_at, finished_at, created_at
		FROM tasks
		WHERE group_id = ? AND status IN ('queued', 'running')
		  AND sla_reminded_at IS NULL AND created_at < ?
		ORDER BY id`, groupID, cutoffStr)
	if err != nil {
		return nil, fmt.Errorf("store: SLA-due tasks of group %d: %w", groupID, err)
	}
	return scanTasks(rows, fmt.Sprintf("store: SLA-due tasks of group %d", groupID))
}

// MarkTaskSLAReminded (S30) stamps the one-time SLA reminder flag on a task
// (UTC RFC3339). Once set, the scheduler never reminds again for that task.
func (s *Store) MarkTaskSLAReminded(id int, ts string) error {
	if _, err := s.db.Exec(
		`UPDATE tasks SET sla_reminded_at = ? WHERE id = ?`, ts, id); err != nil {
		return fmt.Errorf("store: mark task %d SLA reminded: %w", id, err)
	}
	return nil
}

// OpenTasksByGroup counts a group's open tasks (queued + running) for the
// conversation list (S22).
func (s *Store) OpenTasksByGroup(groupID int) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE group_id = ? AND status IN ('queued', 'running')`,
		groupID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: open tasks of group %d: %w", groupID, err)
	}
	return n, nil
}

func scanTasks(rows *sql.Rows, ctx string) ([]Task, error) {
	defer rows.Close()
	out := make([]Task, 0, 8)
	for rows.Next() {
		var t Task
		var proj, wf, grp sql.NullInt64
		var started, finished sql.NullString
		if err := rows.Scan(&t.ID, &proj, &t.EmpID, &wf, &grp, &t.RequestedBy, &t.Kind, &t.Status,
			&t.Priority, &t.Input, &t.Output, &started, &finished, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("%s: %w", ctx, err)
		}
		t.ProjectID = nullInt64(proj)
		t.WorkflowID = nullInt64(wf)
		t.GroupID = nullInt64(grp)
		t.StartedAt = nullString(started)
		t.FinishedAt = nullString(finished)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", ctx, err)
	}
	return out, nil
}

// SetTaskStatus moves a task through the state machine and stamps timestamps.
// done/failed carry output; canceled is allowed from queued and running.
//
// The transition is atomic (S27 M2): the allowedMoves precheck is a
// fast-fail friendly error, but the UPDATE itself is conditioned on the
// status read above (WHERE id = ? AND status = ?), so two concurrent callers
// (e.g. finish racing cancel) cannot both pass the check and overwrite each
// other. When RowsAffected == 0 the row was moved (or deleted) between the
// read and the write; we re-read to answer with a typed error: ErrNotFound
// if the row vanished, ErrInvalidTransition with the actual current state
// otherwise.
func (s *Store) SetTaskStatus(id int, status, output string) error {
	t, err := s.GetTask(id)
	if err != nil {
		return fmt.Errorf("store: set task status: %w", err)
	}
	if !allowedMoves[t.Status][status] {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.Status, status)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var set string
	var args []any
	switch status {
	case TaskRunning:
		set = "status = ?, started_at = ?"
		args = []any{status, now}
	case TaskDone, TaskFailed:
		set = "status = ?, output = ?, finished_at = ?"
		args = []any{status, output, now}
	case TaskCanceled:
		set = "status = ?, output = ?, finished_at = ?"
		args = []any{status, output, now}
	default:
		return fmt.Errorf("%w: %s", ErrInvalidTransition, status)
	}
	args = append(args, id, t.Status)
	res, err := s.db.Exec(`UPDATE tasks SET `+set+` WHERE id = ? AND status = ?`, args...)
	if err != nil {
		return fmt.Errorf("store: set task %d to %s: %w", id, status, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set task %d rows affected: %w", id, err)
	}
	if n > 0 {
		return nil
	}
	// Lost the race: re-read to distinguish a vanished row from a state that
	// no longer matches.
	cur, err := s.GetTask(id)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("store: set task %d status: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: set task %d status: %w", id, err)
	}
	return fmt.Errorf("%w: task %d is %s, not %s", ErrInvalidTransition, id, cur.Status, t.Status)
}

// PreemptTask forcibly returns a running task to the queued state for the
// reason recorded in note. It is a dedicated state-machine bypass (not part
// of allowedMoves) used by the agent preemption flow. Only running tasks may
// be preempted; any other state yields ErrInvalidTransition.
func (s *Store) PreemptTask(id int, note string) error {
	res, err := s.db.Exec(
		`UPDATE tasks SET status = 'queued', started_at = NULL, output = ? WHERE id = ? AND status = 'running'`,
		"preempted: "+note, id)
	if err != nil {
		return fmt.Errorf("store: preempt task %d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	switch n {
	case 0:
		// Distinguish not-found from wrong-state.
		var status string
		err := s.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: preempt task %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: preempt task %d: %w", id, err)
		}
		return fmt.Errorf("%w: task %d is %s, not running", ErrInvalidTransition, id, status)
	default:
		return nil
	}
}

// RequeueRunningTasks moves all running tasks of an employee back to queued.
// Used when an agent re-attaches (initial attach) so orphaned tasks can be
// picked up again. If a task's output is empty the note is stored as-is;
// otherwise the note is appended on a new line. Returns the number of tasks
// requeued.
func (s *Store) RequeueRunningTasks(empID int, note string) (int, error) {
	res, err := s.db.Exec(
		`UPDATE tasks SET status = 'queued', started_at = NULL,
		 output = CASE WHEN output = '' THEN ? ELSE output || char(10) || ? END
		 WHERE employee_id = ? AND status = 'running'`,
		note, note, empID)
	if err != nil {
		return 0, fmt.Errorf("store: requeue running tasks of employee %d: %w", empID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: requeue rows affected: %w", err)
	}
	return int(n), nil
}

// ReassignTask (S27) moves task id to newEmpID and resets it to queued:
// employee_id is swapped, started_at/finished_at are cleared, and the audit
// note is appended to output on its own line (same CASE pattern as
// RequeueRunningTasks).
//
// Allowed source states: queued, running, canceled, failed. done is terminal
// and yields ErrInvalidTransition. The conditional UPDATE pins both the
// status and the owner the caller read (expectedOwner, S27 review R2): the
// API layer authorizes the reassign against the owner it just read, so a
// concurrent reassign that swaps the owner between that read and this write
// must not slip through on the stale permission read. RowsAffected == 0
// means the row vanished or its state/owner moved; we re-read to answer with
// a typed error — ErrNotFound if the row is gone, ErrInvalidTransition
// (naming the actual state and owner) otherwise.
func (s *Store) ReassignTask(id, newEmpID, expectedOwner int, note string) error {
	if _, err := s.GetEmployee(newEmpID); err != nil {
		return fmt.Errorf("store: reassign task %d to employee %d: %w", id, newEmpID, err)
	}
	res, err := s.db.Exec(`
		UPDATE tasks SET employee_id = ?, status = 'queued', started_at = NULL, finished_at = NULL,
		 output = CASE WHEN output = '' THEN ? ELSE output || char(10) || ? END
		 WHERE id = ? AND status IN ('queued', 'running', 'canceled', 'failed') AND employee_id = ?`,
		newEmpID, note, note, id, expectedOwner)
	if err != nil {
		return fmt.Errorf("store: reassign task %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: reassign task %d rows affected: %w", id, err)
	}
	if n > 0 {
		return nil
	}
	// Lost the race: re-read to distinguish a vanished row from a state/owner
	// that no longer matches the caller's read.
	var status string
	var owner int
	err = s.db.QueryRow(`SELECT status, employee_id FROM tasks WHERE id = ?`, id).Scan(&status, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: reassign task %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: reassign task %d: %w", id, err)
	}
	return fmt.Errorf("%w: task %d is %s owned by %d", ErrInvalidTransition, id, status, owner)
}

// TaskStatusCounts returns task counts grouped by status (P1b /metrics).
// Returns status→count for non-zero statuses only; the API layer fills in
// the fixed set (queued/running/done/failed/canceled) with 0 defaults.
func (s *Store) TaskStatusCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("store: task status counts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int, 5)
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: task status counts scan: %w", err)
		}
		out[status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: task status counts rows: %w", err)
	}
	return out, nil
}

// CountEmployeeReviewsInHour (leader orchestration, S1) returns how many
// review tasks were created for empID within the last hour — the throttle
// input for the FinishTask review wake. created_at is stamped by the schema
// default datetime('now') as a UTC "2006-01-02 15:04:05" string, the same
// domain datetime('now','-1 hour') produces, so the comparison is a plain
// lexicographic string compare (S5 lesson: never mix time layouts).
func (s *Store) CountEmployeeReviewsInHour(empID int) (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE employee_id = ? AND kind = 'review' AND created_at >= datetime('now','-1 hour')`,
		empID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count reviews in hour for employee %d: %w", empID, err)
	}
	return n, nil
}
