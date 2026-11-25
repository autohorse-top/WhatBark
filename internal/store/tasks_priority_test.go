package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestCreateTaskPriorityRange (S18): priority must be stored verbatim for
// valid values and rejected outside [0,99].
func TestCreateTaskPriorityRange(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	cases := []struct {
		priority int
		wantErr  bool
	}{
		{0, false},
		{1, false},
		{99, false},
		{-1, true},
		{100, true},
	}
	for _, tc := range cases {
		got, err := s.CreateTask(worker, KindDispatch, "prio", nil, nil, 0, tc.priority, nil)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("priority %d: accepted, want range error", tc.priority)
			}
			continue
		}
		if err != nil {
			t.Fatalf("priority %d: %v", tc.priority, err)
		}
		task, err := s.GetTask(got)
		if err != nil {
			t.Fatalf("get task %d: %v", got, err)
		}
		if task.Priority != tc.priority {
			t.Fatalf("task %d priority = %d, want %d", got, task.Priority, tc.priority)
		}
	}
}

// TestPreemptTask (S18): only running tasks can be preempted; the task goes
// back to queued with started_at cleared and the note recorded in output.
func TestPreemptTask(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	run := func(input string, priority int) int {
		t.Helper()
		id, err := s.CreateTask(worker, KindDispatch, input, nil, nil, 0, priority, nil)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		if err := s.SetTaskStatus(id, TaskRunning, ""); err != nil {
			t.Fatalf("start task %d: %v", id, err)
		}
		return id
	}

	// running -> preempted: queued, started_at cleared, note in output,
	// priority preserved.
	id := run("work", 7)
	if err := s.PreemptTask(id, "task 9 (priority 99)"); err != nil {
		t.Fatalf("preempt running: %v", err)
	}
	task, err := s.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskQueued {
		t.Errorf("status = %s, want queued", task.Status)
	}
	if task.StartedAt != nil {
		t.Errorf("started_at = %s, want nil after preempt", *task.StartedAt)
	}
	if task.Output != "preempted: task 9 (priority 99)" {
		t.Errorf("output = %q, want the preempt note", task.Output)
	}
	if task.Priority != 7 {
		t.Errorf("priority = %d, want 7 (preserved)", task.Priority)
	}

	// wrong-state cases.
	if err := s.PreemptTask(id, "again"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("preempt queued task: err = %v, want ErrInvalidTransition", err)
	}
	done := run("done soon", 0)
	if err := s.SetTaskStatus(done, TaskDone, "out"); err != nil {
		t.Fatal(err)
	}
	if err := s.PreemptTask(done, "x"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("preempt done task: err = %v, want ErrInvalidTransition", err)
	}
	canceled := run("cancel me", 0)
	if err := s.SetTaskStatus(canceled, TaskCanceled, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := s.PreemptTask(canceled, "x"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("preempt canceled task: err = %v, want ErrInvalidTransition", err)
	}
	if err := s.PreemptTask(987654, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("preempt missing task: err = %v, want ErrNotFound", err)
	}
}

// TestRequeueRunningTasks (S18): initial-attach orphan recovery. All running
// tasks of the employee go back to queued; queued/done tasks and other
// employees' tasks are untouched. The note replaces empty output and is
// appended (new line) to non-empty output.
func TestRequeueRunningTasks(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	mk := func(emp int, input string) int {
		t.Helper()
		id, err := s.CreateTask(emp, KindDispatch, input, nil, nil, 0, 0, nil)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		if err := s.SetTaskStatus(id, TaskRunning, ""); err != nil {
			t.Fatalf("start task %d: %v", id, err)
		}
		return id
	}
	withOutput := mk(worker, "partial work")
	// Simulate streamed agent output while running.
	if _, err := s.DB().Exec(`UPDATE tasks SET output = ? WHERE id = ?`, "partial work", withOutput); err != nil {
		t.Fatal(err)
	}
	emptyOutput := mk(worker, "no work yet")
	queuedOnly := func(emp int) int {
		id, err := s.CreateTask(emp, KindDispatch, "still queued", nil, nil, 0, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	untouched := queuedOnly(worker)
	otherEmp := mk(leader, "other employee")

	n, err := s.RequeueRunningTasks(worker, "agent reattached")
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n != 2 {
		t.Fatalf("requeued = %d, want 2", n)
	}

	for _, id := range []int{withOutput, emptyOutput} {
		task, err := s.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != TaskQueued || task.StartedAt != nil {
			t.Errorf("task %d: status=%s started=%v, want queued/nil", id, task.Status, task.StartedAt)
		}
	}
	got, _ := s.GetTask(withOutput)
	if got.Output != "partial work\nagent reattached" {
		t.Errorf("task %d output = %q, want note appended on a new line", withOutput, got.Output)
	}
	got, _ = s.GetTask(emptyOutput)
	if got.Output != "agent reattached" {
		t.Errorf("task %d output = %q, want bare note", emptyOutput, got.Output)
	}
	got, _ = s.GetTask(untouched)
	if got.Status != TaskQueued || got.Output != "" {
		t.Errorf("task %d: queued task disturbed (status=%s output=%q)", untouched, got.Status, got.Output)
	}
	got, _ = s.GetTask(otherEmp)
	if got.Status != TaskRunning {
		t.Errorf("task %d (other employee): status = %s, want still running", otherEmp, got.Status)
	}

	// Second call finds nothing running.
	n, err = s.RequeueRunningTasks(worker, "agent reattached")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second requeue = %d, want 0", n)
	}
}

// TestMigrateTasksPriority (S18): the ALTER-based migration adds the column
// to legacy databases and is idempotent on fresh ones.
func TestMigrateTasksPriority(t *testing.T) {
	// Fresh schema already has the column; re-running is a no-op.
	s := openTestStore(t)
	if err := migrateTasksPriority(s.DB()); err != nil {
		t.Fatalf("re-run on fresh schema: %v", err)
	}

	// Legacy database: a real DB file with the pre-S18 tasks table (no
	// priority column). Opening the store must add the column.
	legacy := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", legacy)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	legacySchema := `
		 CREATE TABLE tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER,
			employee_id INTEGER NOT NULL,
			workflow_id INTEGER,
			requested_by INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL DEFAULT 'dispatch',
			status TEXT NOT NULL DEFAULT 'queued',
			input TEXT NOT NULL DEFAULT '',
			output TEXT NOT NULL DEFAULT '',
			started_at TEXT,
			finished_at TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		 );`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy tasks: %v", err)
	}
	if err := migrateTasksPriority(db); err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'priority'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("priority column count = %d, want 1", n)
	}
	// Idempotent: a second run must not fail or duplicate.
	if err := migrateTasksPriority(db); err != nil {
		t.Fatalf("re-run on migrated legacy: %v", err)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'priority'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("after re-run, priority column count = %d, want 1", n)
	}
}
