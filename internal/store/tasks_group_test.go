package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

// TestCreateTaskWithGroup (S22): group-dispatched tasks persist group_id on
// every read surface, plain 1:1 tasks keep it nil, and unknown groups are
// rejected.
func TestCreateTaskWithGroup(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	gid, err := s.CreateGroup("task-group", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	gidp := intp(gid)

	tid, err := s.CreateTask(worker, KindDispatch, "do it in the group", nil, nil, leader, 0, gidp)
	if err != nil {
		t.Fatalf("CreateTask with group: %v", err)
	}
	tk, err := s.GetTask(tid)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.GroupID == nil || *tk.GroupID != gid {
		t.Fatalf("task.group_id = %v, want %d", tk.GroupID, gid)
	}
	if tk.RequestedBy != leader {
		t.Fatalf("requested_by = %d, want %d", tk.RequestedBy, leader)
	}

	byEmp, err := s.ListTasksByEmployee(worker, nil)
	if err != nil || len(byEmp) != 1 || byEmp[0].GroupID == nil || *byEmp[0].GroupID != gid {
		t.Fatalf("ListTasksByEmployee = %+v (err %v), want one task with group %d", byEmp, err, gid)
	}
	all, err := s.ListTasks(nil)
	if err != nil || len(all) != 1 || all[0].GroupID == nil || *all[0].GroupID != gid {
		t.Fatalf("ListTasks = %+v (err %v), want the group task", all, err)
	}

	plain, err := s.CreateTask(worker, KindDispatch, "1:1 task", nil, nil, 0, 0, nil)
	if err != nil {
		t.Fatalf("CreateTask plain: %v", err)
	}
	pt, err := s.GetTask(plain)
	if err != nil || pt.GroupID != nil {
		t.Fatalf("plain task = %+v (err %v), want nil group_id", pt, err)
	}

	if _, err := s.CreateTask(worker, KindDispatch, "bad group", nil, nil, leader, 0, intp(99999)); err == nil {
		t.Fatal("CreateTask with unknown group accepted, want error")
	}
}

// TestOpenTasksByGroup (S22): only queued+running tasks of the given group
// count; done/failed/canceled, other groups and 1:1 tasks do not.
func TestOpenTasksByGroup(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	g1, err := s.CreateGroup("open-g1", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup g1: %v", err)
	}
	g2, err := s.CreateGroup("open-g2", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup g2: %v", err)
	}
	newTask := func(g *int) int {
		t.Helper()
		id, err := s.CreateTask(worker, KindDispatch, "x", nil, nil, leader, 0, g)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		return id
	}
	newTask(intp(g1))
	newTask(intp(g1))
	running := newTask(intp(g1))
	done := newTask(intp(g1))
	newTask(intp(g2))
	newTask(nil) // 1:1 task, no group

	mustMove := func(id int, status string) {
		t.Helper()
		if err := s.SetTaskStatus(id, status, ""); err != nil {
			t.Fatalf("SetTaskStatus %s: %v", status, err)
		}
	}
	mustMove(running, TaskRunning)
	mustMove(done, TaskRunning)
	if err := s.SetTaskStatus(done, TaskDone, "done"); err != nil {
		t.Fatalf("finish done task: %v", err)
	}

	if got, err := s.OpenTasksByGroup(g1); err != nil || got != 3 {
		t.Fatalf("OpenTasksByGroup(g1) = %d (err %v), want 3", got, err)
	}
	if got, err := s.OpenTasksByGroup(g2); err != nil || got != 1 {
		t.Fatalf("OpenTasksByGroup(g2) = %d (err %v), want 1", got, err)
	}
	if got, err := s.OpenTasksByGroup(99999); err != nil || got != 0 {
		t.Fatalf("OpenTasksByGroup(missing group) = %d (err %v), want 0", got, err)
	}
}

// TestListGroupTasks (S24): table-driven — empty group, id-desc ordering,
// limit truncation, and cross-group isolation.
func TestListGroupTasks(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	g1, err := s.CreateGroup("board-g1", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup g1: %v", err)
	}
	g2, err := s.CreateGroup("board-g2", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup g2: %v", err)
	}
	newTask := func(g *int, input string) int {
		t.Helper()
		id, err := s.CreateTask(worker, KindDispatch, input, nil, nil, leader, 0, g)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		return id
	}
	t1 := newTask(intp(g1), "a")
	t2 := newTask(intp(g1), "b")
	t3 := newTask(intp(g1), "c")
	t4 := newTask(intp(g2), "other")
	newTask(nil, "1:1") // group-less task must never appear in any group board

	tests := []struct {
		name  string
		gid   int
		limit int
		want  []int
	}{
		{"empty group", 99999, 50, []int{}},
		{"cross-group isolation", g2, 50, []int{t4}},
		{"id desc order", g1, 50, []int{t3, t2, t1}},
		{"limit truncates to newest", g1, 2, []int{t3, t2}},
		{"limit above count returns all", g1, 500, []int{t3, t2, t1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListGroupTasks(tc.gid, tc.limit, nil)
			if err != nil {
				t.Fatalf("ListGroupTasks(%d,%d,nil): %v", tc.gid, tc.limit, err)
			}
			ids := make([]int, 0, len(got))
			for _, tk := range got {
				ids = append(ids, tk.ID)
				if tc.gid != 99999 && (tk.GroupID == nil || *tk.GroupID != tc.gid) {
					t.Fatalf("task %d group = %v, want %d", tk.ID, tk.GroupID, tc.gid)
				}
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

// TestListGroupTasksProjectFilter (S29): the board's optional project filter —
// nil returns all group tasks, 0 only project-less tasks (project_id IS NULL),
// N>0 only that project's tasks; unknown projects and unknown groups yield
// empty slices.
func TestListGroupTasksProjectFilter(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	p1, err := s.CreateProject("PF-1", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateProject p1: %v", err)
	}
	p2, err := s.CreateProject("PF-2", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateProject p2: %v", err)
	}
	gid, err := s.CreateGroup("pf-group", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	newTask := func(proj *int, input string) int {
		t.Helper()
		id, err := s.CreateTask(worker, KindDispatch, input, proj, nil, leader, 0, intp(gid))
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		return id
	}
	tA := newTask(intp(p1), "a")
	tB := newTask(intp(p2), "b")
	tC := newTask(nil, "c")
	tD := newTask(intp(p1), "d")

	tests := []struct {
		name string
		gid  int
		proj *int
		want []int
	}{
		{"nil: all group tasks", gid, nil, []int{tD, tC, tB, tA}},
		{"0: only project-less tasks", gid, intp(0), []int{tC}},
		{"p1: only project 1 tasks", gid, intp(p1), []int{tD, tA}},
		{"p2: only project 2 tasks", gid, intp(p2), []int{tB}},
		{"unknown project: empty", gid, intp(99999), []int{}},
		{"empty group: nil filter", 99999, nil, []int{}},
		{"empty group: 0 filter", 99999, intp(0), []int{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListGroupTasks(tc.gid, 50, tc.proj)
			if err != nil {
				t.Fatalf("ListGroupTasks(%d, 50, %v): %v", tc.gid, tc.proj, err)
			}
			ids := make([]int, 0, len(got))
			for _, tk := range got {
				ids = append(ids, tk.ID)
				if tc.gid != 99999 && (tk.GroupID == nil || *tk.GroupID != tc.gid) {
					t.Fatalf("task %d group = %v, want %d", tk.ID, tk.GroupID, tc.gid)
				}
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

// TestDispatchMessageIDForGroupTask (S24): the dispatch anchor message lookup
// used by the group task board — hit, no-anchor miss, cross-group and
// non-dispatch-kind all must not cross-reference.
func TestDispatchMessageIDForGroupTask(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	g1, err := s.CreateGroup("anchor-g1", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup g1: %v", err)
	}
	g2, err := s.CreateGroup("anchor-g2", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup g2: %v", err)
	}
	linkDispatch := func(gid int, body string) (taskID, msgID int) {
		t.Helper()
		var err error
		taskID, err = s.CreateTask(worker, KindDispatch, body, nil, nil, leader, 0, intp(gid))
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		msgID, err = s.CreateGroupMessage(gid, MsgDispatch, body, intp(leader), nil, nil)
		if err != nil {
			t.Fatalf("CreateGroupMessage: %v", err)
		}
		if err := s.SetMessageTaskID(msgID, taskID); err != nil {
			t.Fatalf("SetMessageTaskID: %v", err)
		}
		return taskID, msgID
	}
	taskA, msgA := linkDispatch(g1, "A")
	taskB, msgB := linkDispatch(g2, "B")

	// taskC: a group task with no dispatch message (report only).
	taskC, err := s.CreateTask(worker, KindDispatch, "no anchor", nil, nil, leader, 0, intp(g1))
	if err != nil {
		t.Fatalf("CreateTask C: %v", err)
	}
	rep, err := s.CreateGroupMessage(g1, MsgReport, "c done", intp(leader), nil, nil)
	if err != nil {
		t.Fatalf("CreateGroupMessage report: %v", err)
	}
	if err := s.SetMessageTaskID(rep, taskC); err != nil {
		t.Fatalf("SetMessageTaskID report: %v", err)
	}

	tests := []struct {
		name string
		gid  int
		task int
		want int
	}{
		{"hit in g1", g1, taskA, msgA},
		{"hit in g2", g2, taskB, msgB},
		{"task without dispatch message", g1, taskC, 0},
		{"cross-group: g2 asked for g1 task", g2, taskA, 0},
		{"cross-group: g1 asked for g2 task", g1, taskB, 0},
		{"non-dispatch kind does not match", g1, taskC, 0},
		{"unknown task", g1, 99999, 0},
		{"unknown group", 99999, taskA, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.DispatchMessageIDForGroupTask(tc.gid, tc.task)
			if err != nil {
				t.Fatalf("DispatchMessageIDForGroupTask(%d,%d): %v", tc.gid, tc.task, err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestMigrateTasksGroup (S22): pre-S22 databases (tasks table without
// group_id) get the column on Open, keep their rows, and re-opening is a
// no-op.
func TestMigrateTasksGroup(t *testing.T) {
	// Fresh schema already has the column; re-running is a no-op.
	s := openTestStore(t)
	if err := migrateTasksGroup(s.DB()); err != nil {
		t.Fatalf("re-run on fresh schema: %v", err)
	}

	legacy := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", legacy)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	legacySchema := `
	 CREATE TABLE tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id INTEGER,
		employee_id INTEGER NOT NULL,
		workflow_id INTEGER,
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
		t.Fatalf("create legacy tasks: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, employee_id, kind, status, priority, input)
		VALUES (1, 1, 'dispatch', 'queued', 0, 'legacy input')`); err != nil {
		t.Fatalf("seed legacy task: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	st, err := Open(legacy)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'group_id'`).Scan(&n); err != nil {
		t.Fatalf("inspect tasks: %v", err)
	}
	if n != 1 {
		t.Fatalf("group_id column count = %d, want 1", n)
	}
	var grp sql.NullInt64
	var input string
	if err := st.DB().QueryRow(`SELECT group_id, input FROM tasks WHERE id = 1`).Scan(&grp, &input); err != nil {
		t.Fatalf("read legacy task: %v", err)
	}
	if grp.Valid || input != "legacy input" {
		t.Fatalf("legacy task = group %v input %q, want NULL group and original input", grp, input)
	}
	// Since the kind-review migration (S1) the upgraded tasks table carries
	// the full base-schema shape — group_id has a real REFERENCES groups(id)
	// FK — so the write probe needs an existing group row. The legacy DB has
	// no employees, so the group row is seeded with raw SQL (boss-created).
	if _, err := st.DB().Exec(`INSERT INTO groups (name, created_by) VALUES ('probe-group', 0)`); err != nil {
		t.Fatalf("seed probe group: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE tasks SET group_id = (SELECT id FROM groups LIMIT 1) WHERE id = 1`); err != nil {
		t.Fatalf("write migrated column: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, err := Open(legacy)
	if err != nil {
		t.Fatalf("re-open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	if err := st2.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'group_id'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("after re-open, group_id column count = %d, want 1", n)
	}
}
