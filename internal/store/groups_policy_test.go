package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// backdateTask sets a task's created_at to a fixed UTC 'YYYY-MM-DD HH:MM:SS'
// value so SLA-due cutoff tests are deterministic.
func backdateTask(t *testing.T, s *Store, id int, ts time.Time) {
	t.Helper()
	if _, err := s.DB().Exec(
		`UPDATE tasks SET created_at = ? WHERE id = ?`,
		ts.UTC().Format("2006-01-02 15:04:05"), id); err != nil {
		t.Fatalf("backdate task %d: %v", id, err)
	}
}

// TestGroupPolicyMigration (S30): migrateGroupsPolicy adds the three policy
// columns to a pre-S30 database and is a no-op on a fresh one.
func TestGroupPolicyMigration(t *testing.T) {
	// Fresh schema already carries all three columns; re-running is a no-op.
	s := openTestStore(t)
	if err := migrateGroupsPolicy(s.DB()); err != nil {
		t.Fatalf("re-run on fresh schema: %v", err)
	}

	// Legacy database: groups and tasks tables without the S30 columns.
	legacy := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", legacy)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	legacySchema := `
		CREATE TABLE groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			created_by INTEGER NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			kind TEXT NOT NULL DEFAULT 'group'
		);
		CREATE TABLE tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER,
			employee_id INTEGER NOT NULL,
			workflow_id INTEGER,
			group_id INTEGER,
			requested_by INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL DEFAULT 'dispatch',
			status TEXT NOT NULL DEFAULT 'queued',
			priority INTEGER NOT NULL DEFAULT 0,
			input TEXT NOT NULL DEFAULT '',
			output TEXT NOT NULL DEFAULT '',
			started_at TEXT,
			finished_at TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy tables: %v", err)
	}
	if err := migrateGroupsPolicy(db); err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	for _, col := range []string{"default_priority", "sla_timeout_seconds"} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('groups') WHERE name = ?`, col).Scan(&n); err != nil {
			t.Fatalf("inspect groups.%s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("groups.%s column count = %d, want 1", col, n)
		}
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'sla_reminded_at'`).Scan(&n); err != nil {
		t.Fatalf("inspect tasks.sla_reminded_at: %v", err)
	}
	if n != 1 {
		t.Errorf("tasks.sla_reminded_at column count = %d, want 1", n)
	}
	// Idempotent: a second run must not fail or duplicate.
	if err := migrateGroupsPolicy(db); err != nil {
		t.Fatalf("re-run on migrated legacy: %v", err)
	}
}

// TestSetGroupPolicy (S30): COALESCE partial update — a nil pointer leaves
// that field unchanged; both-set works; unknown group is ErrNotFound.
func TestSetGroupPolicy(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)
	gid, err := s.CreateGroup("s30", leader, []int{worker}, "group")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	ctx := context.Background()

	// Fresh group: both policy fields are 0.
	if g, err := s.GetGroup(gid); err != nil || g.DefaultPriority != 0 || g.SlaTimeoutSeconds != 0 {
		t.Fatalf("fresh group = %+v (err %v), want dp=0 sla=0", g, err)
	}

	// Set only default_priority.
	if err := s.SetGroupPolicy(ctx, gid, intp(42), nil); err != nil {
		t.Fatalf("set dp: %v", err)
	}
	g, err := s.GetGroup(gid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if g.DefaultPriority != 42 || g.SlaTimeoutSeconds != 0 {
		t.Errorf("after set dp: dp=%d sla=%d, want 42/0", g.DefaultPriority, g.SlaTimeoutSeconds)
	}

	// Set only sla_timeout_seconds; dp must persist.
	if err := s.SetGroupPolicy(ctx, gid, nil, intp(300)); err != nil {
		t.Fatalf("set sla: %v", err)
	}
	g, err = s.GetGroup(gid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if g.DefaultPriority != 42 || g.SlaTimeoutSeconds != 300 {
		t.Errorf("after set sla: dp=%d sla=%d, want 42/300", g.DefaultPriority, g.SlaTimeoutSeconds)
	}

	// Both at once, including sla back to 0 (disabled).
	if err := s.SetGroupPolicy(ctx, gid, intp(7), intp(0)); err != nil {
		t.Fatalf("set both: %v", err)
	}
	g, _ = s.GetGroup(gid)
	if g.DefaultPriority != 7 || g.SlaTimeoutSeconds != 0 {
		t.Errorf("after set both: dp=%d sla=%d, want 7/0", g.DefaultPriority, g.SlaTimeoutSeconds)
	}

	// Unknown group is ErrNotFound.
	if err := s.SetGroupPolicy(ctx, 99999, intp(1), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown group: err=%v, want ErrNotFound", err)
	}
}

// TestListGroupsPolicy (S30): ListGroups returns the policy columns for every
// group, ordered by id.
func TestListGroupsPolicy(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)
	g1, err := s.CreateGroup("a", leader, []int{worker}, "group")
	if err != nil {
		t.Fatalf("g1: %v", err)
	}
	g2, err := s.CreateGroup("b", leader, []int{worker}, "group")
	if err != nil {
		t.Fatalf("g2: %v", err)
	}
	ctx := context.Background()
	if err := s.SetGroupPolicy(ctx, g1, intp(10), intp(120)); err != nil {
		t.Fatalf("set g1: %v", err)
	}
	if err := s.SetGroupPolicy(ctx, g2, intp(55), intp(0)); err != nil {
		t.Fatalf("set g2: %v", err)
	}

	groups, err := s.ListGroups()
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("ListGroups = %d rows, want 2: %+v", len(groups), groups)
	}
	if groups[0].ID != g1 || groups[0].DefaultPriority != 10 || groups[0].SlaTimeoutSeconds != 120 {
		t.Errorf("row 0 = %+v, want id=%d dp=10 sla=120", groups[0], g1)
	}
	if groups[1].ID != g2 || groups[1].DefaultPriority != 55 || groups[1].SlaTimeoutSeconds != 0 {
		t.Errorf("row 1 = %+v, want id=%d dp=55 sla=0", groups[1], g2)
	}
}

// TestListOpenTasksSLADue (S30): only open (queued/running) tasks that are
// unreminded and older than cutoff are returned; marked/done/fresh tasks are
// excluded.
func TestListOpenTasksSLADue(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)
	gid, err := s.CreateGroup("sla", leader, []int{worker}, "group")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	old := time.Now().UTC().Add(-10 * time.Minute)
	// A queued and a running task, both backdated past the cutoff.
	queued, err := s.CreateTask(worker, KindDispatch, "old queued", nil, nil, leader, 0, intp(gid))
	if err != nil {
		t.Fatalf("queued: %v", err)
	}
	backdateTask(t, s, queued, old)
	running, err := s.CreateTask(worker, KindDispatch, "old running", nil, nil, leader, 0, intp(gid))
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := s.SetTaskStatus(running, TaskRunning, ""); err != nil {
		t.Fatalf("start running: %v", err)
	}
	backdateTask(t, s, running, old)
	// A done task backdated past the cutoff — must be excluded (not open).
	done, err := s.CreateTask(worker, KindDispatch, "old done", nil, nil, leader, 0, intp(gid))
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if err := s.SetTaskStatus(done, TaskRunning, ""); err != nil {
		t.Fatalf("start done: %v", err)
	}
	if err := s.SetTaskStatus(done, TaskDone, "x"); err != nil {
		t.Fatalf("finish done: %v", err)
	}
	backdateTask(t, s, done, old)
	// A fresh queued task — created now, so after the cutoff.
	fresh, err := s.CreateTask(worker, KindDispatch, "fresh", nil, nil, leader, 0, intp(gid))
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}

	cutoff := time.Now().UTC().Add(-5 * time.Minute)
	due, err := s.ListOpenTasksSLADue(gid, cutoff)
	if err != nil {
		t.Fatalf("ListOpenTasksSLADue: %v", err)
	}
	got := map[int]bool{}
	for _, task := range due {
		got[task.ID] = true
	}
	if !got[queued] || !got[running] {
		t.Errorf("due = %v, want it to include queued %d and running %d", got, queued, running)
	}
	if got[done] {
		t.Error("done task must not be SLA-due")
	}
	if got[fresh] {
		t.Error("fresh task must not be SLA-due (created after cutoff)")
	}

	// Mark the queued task reminded -> it drops out of the due set.
	if err := s.MarkTaskSLAReminded(queued, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("MarkTaskSLAReminded: %v", err)
	}
	var ts string
	if err := s.DB().QueryRow(
		`SELECT sla_reminded_at FROM tasks WHERE id = ?`, queued).Scan(&ts); err != nil {
		t.Fatalf("read sla_reminded_at: %v", err)
	}
	if ts == "" {
		t.Error("sla_reminded_at not set after MarkTaskSLAReminded")
	}
	due, err = s.ListOpenTasksSLADue(gid, cutoff)
	if err != nil {
		t.Fatalf("ListOpenTasksSLADue (2nd): %v", err)
	}
	for _, task := range due {
		if task.ID == queued {
			t.Error("reminded task still SLA-due")
		}
	}
}
