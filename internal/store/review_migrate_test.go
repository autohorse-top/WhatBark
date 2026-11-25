package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// legacyPreReviewTasks is the tasks DDL of a database created just before
// the review kind: current 15-column shape, CHECK without 'review'. This is
// what real pre-review deployments carry (every older column was already
// migrated by the additive steps that run before migrateTasksKindReview).
const legacyPreReviewTasks = `
CREATE TABLE tasks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	employee_id INTEGER NOT NULL REFERENCES employees(id),
	workflow_id INTEGER REFERENCES workflows(id) ON DELETE SET NULL,
	group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
	requested_by INTEGER NOT NULL DEFAULT 0,
	kind TEXT NOT NULL DEFAULT 'dispatch'
		CHECK (kind IN ('dispatch', 'ask', 'scheduled', 'selfreview')),
	status TEXT NOT NULL DEFAULT 'queued'
		CHECK (status IN ('queued', 'running', 'done', 'failed', 'canceled')),
	priority INTEGER NOT NULL DEFAULT 0,
	input TEXT NOT NULL DEFAULT '',
	output TEXT NOT NULL DEFAULT '',
	started_at TEXT,
	finished_at TEXT,
	sla_reminded_at TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
)`

const legacyPreReviewMessages = `
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	from_id INTEGER REFERENCES employees(id),
	to_id INTEGER REFERENCES employees(id),
	kind TEXT NOT NULL CHECK (kind IN ('report', 'dispatch', 'ask', 'answer', 'chat', 'summary', 'system', 'scheduled', 'selfreview')),
	body TEXT NOT NULL,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
	group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
	reply_to INTEGER,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
)`

const legacyPreReviewEmployees = `
CREATE TABLE employees (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	role TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'ai' CHECK (kind IN ('ai', 'human')),
	department_id INTEGER NOT NULL,
	tool TEXT NOT NULL DEFAULT 'mock',
	tool_args TEXT NOT NULL DEFAULT '{}',
	workdir TEXT NOT NULL DEFAULT '',
	token_hash TEXT NOT NULL DEFAULT '',
	last_seen_at TEXT,
	avatar TEXT NOT NULL DEFAULT '',
	email TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
)`

// seedPreReviewDB opens a raw legacy database (tasks + messages with the
// pre-review CHECK) and seeds one employee, two tasks, and two messages so
// the rebuild's full-column copy has real rows to preserve.
func seedPreReviewDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	for _, ddl := range []string{legacyPreReviewEmployees, legacyPreReviewTasks, legacyPreReviewMessages} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create legacy table: %v", err)
		}
	}
	seed := []string{
		`INSERT INTO employees (id, name, role, kind, department_id) VALUES (1, 'alice', 'leader', 'ai', 1)`,
		`INSERT INTO tasks (id, employee_id, requested_by, kind, status, priority, input, output)
			VALUES (1, 1, 0, 'dispatch', 'done', 5, 'legacy input', 'legacy output')`,
		`INSERT INTO tasks (id, employee_id, requested_by, kind, status, input)
			VALUES (2, 1, 0, 'ask', 'queued', 'legacy ask')`,
		`INSERT INTO messages (id, from_id, to_id, kind, body, task_id)
			VALUES (1, 1, 1, 'chat', 'legacy chat', NULL)`,
		`INSERT INTO messages (id, from_id, to_id, kind, body, task_id)
			VALUES (2, NULL, 1, 'system', 'legacy system', 1)`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed legacy row: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}
	return path
}

// TestMigrateKindReview_LegacyRebuild (F2): a pre-review database is
// upgraded on Open — review tasks/messages insert, legacy rows keep their
// ids and values, the rebuilt CHECKs still reject invalid rows, the base
// schema recreates the dropped indexes, and a second Open is a no-op.
func TestMigrateKindReview_LegacyRebuild(t *testing.T) {
	path := seedPreReviewDB(t)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}

	// Review rows insert into both rebuilt tables.
	revTask, err := st.CreateTask(1, KindReview, "review wake input", nil, nil, 0, 0, nil)
	if err != nil {
		t.Fatalf("CreateTask kind=review on migrated db: %v", err)
	}
	revMsg, err := st.CreateMessage(1, MsgReview, "review wake body", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateMessage kind=review on migrated db: %v", err)
	}
	if revTask == 0 || revMsg == 0 {
		t.Fatalf("review ids = %d/%d, want nonzero", revTask, revMsg)
	}

	// Legacy rows survived the rebuild with ids and payload intact.
	lt, err := st.GetTask(1)
	if err != nil {
		t.Fatalf("get legacy task: %v", err)
	}
	if lt.Kind != KindDispatch || lt.Status != TaskDone || lt.Priority != 5 ||
		lt.Input != "legacy input" || lt.Output != "legacy output" || lt.RequestedBy != 0 {
		t.Fatalf("legacy task 1 = %+v", lt)
	}
	lt2, err := st.GetTask(2)
	if err != nil || lt2.Kind != KindAsk || lt2.Input != "legacy ask" {
		t.Fatalf("legacy task 2 = %+v (err %v)", lt2, err)
	}
	lm, err := st.GetMessage(2)
	if err != nil {
		t.Fatalf("get legacy message: %v", err)
	}
	if lm.Kind != MsgSystem || lm.Body != "legacy system" || lm.FromID != nil || lm.ToID == nil || *lm.ToID != 1 {
		t.Fatalf("legacy message 2 = %+v", lm)
	}
	if lm.TaskID == nil || *lm.TaskID != 1 {
		t.Fatalf("legacy message task_id = %v, want 1", lm.TaskID)
	}

	// The rebuilt tasks DDL still enforces its CHECKs.
	if _, err := st.DB().Exec(
		`INSERT INTO tasks (employee_id, kind, status) VALUES (1, 'dispatch', 'bogus')`); err == nil {
		t.Fatal("invalid status accepted after rebuild, want CHECK error")
	}
	if _, err := st.DB().Exec(
		`INSERT INTO tasks (employee_id, kind, status) VALUES (1, 'bogus', 'queued')`); err == nil {
		t.Fatal("invalid task kind accepted after rebuild, want CHECK error")
	}
	if _, err := st.DB().Exec(
		`INSERT INTO messages (from_id, to_id, kind, body) VALUES (NULL, 1, 'bogus', 'x')`); err == nil {
		t.Fatal("invalid message kind accepted after rebuild, want CHECK error")
	}

	// Indexes dropped with the old tables were recreated by the base
	// schema's CREATE INDEX IF NOT EXISTS statements on Open.
	wantIdx := []string{"idx_tasks_employee_status", "idx_tasks_project",
		"idx_messages_to", "idx_messages_project", "idx_messages_group"}
	for _, idx := range wantIdx {
		var n int
		if err := st.DB().QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil {
			t.Fatalf("inspect index %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("index %s count = %d, want 1", idx, n)
		}
	}

	// Idempotent second Open: the guard sees 'review' in both DDLs.
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	lacks, err := ddlLacksReview(st2.DB(), "tasks")
	if err != nil || lacks {
		t.Fatalf("after migration tasks DDL lacks review = %v (err %v), want false", lacks, err)
	}
	lacks, err = ddlLacksReview(st2.DB(), "messages")
	if err != nil || lacks {
		t.Fatalf("after migration messages DDL lacks review = %v (err %v), want false", lacks, err)
	}
	if _, err := st2.CreateTask(1, KindReview, "review again", nil, nil, 0, 0, nil); err != nil {
		t.Fatalf("CreateTask kind=review after re-open: %v", err)
	}
	if got, err := st2.CountEmployeeReviewsInHour(1); err != nil || got != 2 {
		t.Fatalf("reviews in hour after re-open = %d (err %v), want 2", got, err)
	}
}

// seedPreReviewDBWithNewMessages seeds the other half-open window: the
// messages table was already rebuilt (post-S14 shape, its CHECK already
// carries 'review' — exactly what a pre-S14 database rebuilt by the new
// migrateMessagesGroup DDL looks like), but tasks still has the old CHECK.
// Only tasks must be rebuilt here; messages must survive untouched.
func seedPreReviewDBWithNewMessages(t *testing.T) (path string, msgID int) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "legacy-tasks-only.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	for _, ddl := range []string{legacyPreReviewEmployees, legacyPreReviewTasks, `
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	from_id INTEGER REFERENCES employees(id),
	to_id INTEGER REFERENCES employees(id),
	kind TEXT NOT NULL CHECK (kind IN ('report', 'dispatch', 'ask', 'answer', 'chat', 'summary', 'system', 'scheduled', 'selfreview', 'review')),
	body TEXT NOT NULL,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
	group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
	reply_to INTEGER,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
)`} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create legacy table: %v", err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO employees (id, name, role, kind, department_id) VALUES (1, 'alice', 'leader', 'ai', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (id, employee_id, requested_by, kind, status, input) VALUES (1, 1, 0, 'ask', 'queued', 'legacy ask')`); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(
		`INSERT INTO messages (from_id, to_id, kind, body, task_id) VALUES (NULL, 1, 'review', 'pre-existing wake', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path, int(id)
}

// TestMigrateKindReview_TasksOnly: when only tasks carries the old CHECK
// (messages already has 'review'), the migration rebuilds exactly the tasks
// table and leaves messages — including a pre-existing review message —
// fully intact.
func TestMigrateKindReview_TasksOnly(t *testing.T) {
	path, msgID := seedPreReviewDBWithNewMessages(t)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.CreateTask(1, KindReview, "new review", nil, nil, 0, 0, nil); err != nil {
		t.Fatalf("CreateTask review after tasks-only rebuild: %v", err)
	}
	// The pre-existing review message survived untouched (same row, same
	// body, same task link).
	m, err := st.GetMessage(msgID)
	if err != nil {
		t.Fatalf("get review message: %v", err)
	}
	if m.Kind != MsgReview || m.Body != "pre-existing wake" || m.TaskID == nil || *m.TaskID != 1 {
		t.Fatalf("pre-existing review message = %+v", m)
	}
	// The legacy task kept its id and input through the tasks rebuild.
	tk, err := st.GetTask(1)
	if err != nil || tk.Input != "legacy ask" || tk.Kind != KindAsk {
		t.Fatalf("legacy task = %+v (err %v)", tk, err)
	}
	// And the rebuilt messages CHECK still rejects junk.
	if _, err := st.DB().Exec(
		`INSERT INTO messages (from_id, to_id, kind, body) VALUES (NULL, 1, 'bogus', 'x')`); err == nil {
		t.Fatal("invalid message kind accepted, want CHECK error")
	}
}

// TestMigrateKindReview_FreshDB (F2): a fresh database accepts review rows
// straight away, and running the migration step directly is a no-op (the
// guard hits because the base schema already carries 'review').
func TestMigrateKindReview_FreshDB(t *testing.T) {
	s := openTestStore(t)
	emp := seedOrgEmployee(t, s)

	taskID, err := s.CreateTask(emp, KindReview, "fresh review", nil, nil, 0, 0, nil)
	if err != nil {
		t.Fatalf("CreateTask kind=review on fresh db: %v", err)
	}
	if _, err := s.CreateMessage(emp, MsgReview, "fresh review msg", nil, nil, nil, nil); err != nil {
		t.Fatalf("CreateMessage kind=review on fresh db: %v", err)
	}
	got, err := s.GetTask(taskID)
	if err != nil || got.Kind != KindReview {
		t.Fatalf("fresh review task = %+v (err %v)", got, err)
	}
	if err := migrateTasksKindReview(s.DB()); err != nil {
		t.Fatalf("re-run on fresh schema: %v", err)
	}
	lacks, err := ddlLacksReview(s.DB(), "tasks")
	if err != nil || lacks {
		t.Fatalf("fresh tasks DDL lacks review = %v (err %v), want false", lacks, err)
	}
}

// seedOrgEmployee creates a lone employee and returns its id (review tests
// only need a legal tasks.employee_id target).
func seedOrgEmployee(t *testing.T, s *Store) int {
	t.Helper()
	dept, err := s.CreateDepartment("review-dept", nil)
	if err != nil {
		t.Fatalf("create dept: %v", err)
	}
	emp, err := s.CreateEmployee("reviewer", "lead", KindAI, dept)
	if err != nil {
		t.Fatalf("create employee: %v", err)
	}
	return emp
}

// TestCountEmployeeReviewsInHour: only review tasks, only this employee,
// only within the last hour feed the wake throttle.
func TestCountEmployeeReviewsInHour(t *testing.T) {
	s := openTestStore(t)
	dept, _ := s.CreateDepartment("D", nil)
	leader, err := s.CreateEmployee("count-leader", "lead", KindAI, dept)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateEmployee("count-other", "role", KindAI, dept)
	if err != nil {
		t.Fatal(err)
	}

	mk := func(emp int, kind string) int {
		t.Helper()
		id, err := s.CreateTask(emp, kind, "x", nil, nil, 0, 0, nil)
		if err != nil {
			t.Fatalf("CreateTask %s: %v", kind, err)
		}
		return id
	}
	fresh1 := mk(leader, KindReview)
	fresh2 := mk(leader, KindReview)
	mk(leader, KindDispatch) // dispatches never count
	mk(other, KindReview)    // other employees never count

	tests := []struct {
		name string
		emp  int
		want int
	}{
		{"two recent reviews", leader, 2},
		{"none", other, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.CountEmployeeReviewsInHour(tc.emp)
			if err != nil {
				t.Fatalf("CountEmployeeReviewsInHour(%d): %v", tc.emp, err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}

	// Backdate one review beyond the hour window: it drops out.
	old := "2020-01-01 00:00:00"
	if _, err := s.DB().Exec(`UPDATE tasks SET created_at = ? WHERE id = ?`, old, fresh1); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CountEmployeeReviewsInHour(leader); err != nil || got != 1 {
		t.Fatalf("after backdate = %d (err %v), want 1 (only %d fresh)", got, err, fresh2)
	}
}

// TestCreateTaskKindWhitelist: the Go-layer CreateTask whitelist accepts
// review and still rejects unknown kinds (defense before the SQL CHECK).
func TestCreateTaskKindWhitelist(t *testing.T) {
	s := openTestStore(t)
	emp := seedOrgEmployee(t, s)

	if _, err := s.CreateTask(emp, KindReview, "ok", nil, nil, 0, 0, nil); err != nil {
		t.Fatalf("CreateTask review: %v", err)
	}
	if _, err := s.CreateTask(emp, "nope", "x", nil, nil, 0, 0, nil); err == nil {
		t.Fatal("CreateTask unknown kind accepted")
	} else if !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("err = %v, want unsupported kind", err)
	}
}
