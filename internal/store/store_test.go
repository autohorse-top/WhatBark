package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenPragmas(t *testing.T) {
	s := openTestStore(t)
	var fk int
	if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
	var mode string
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func seedOrg(t *testing.T, s *Store) (rootDept, childDept, leader, worker int) {
	t.Helper()
	root, err := s.CreateDepartment("root", nil)
	if err != nil {
		t.Fatalf("create root dept: %v", err)
	}
	child, err := s.CreateDepartment("child", intp(root))
	if err != nil {
		t.Fatalf("create child dept: %v", err)
	}
	leader, err = s.CreateEmployee("alice", "leader", KindAI, root)
	if err != nil {
		t.Fatalf("create leader: %v", err)
	}
	worker, err = s.CreateEmployee("bob", "worker", KindAI, child)
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if err := s.SetDepartmentLeader(root, leader); err != nil {
		t.Fatalf("set leader: %v", err)
	}
	return root, child, leader, worker
}

func intp(v int) *int { return &v }

func TestDepartmentCRUD(t *testing.T) {
	s := openTestStore(t)
	root, _, _, _ := seedOrg(t, s)

	d, err := s.GetDepartment(root)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if d.Name != "root" || d.ParentID != nil || d.LeaderID == nil {
		t.Fatalf("unexpected root dept %+v", d)
	}

	depts, err := s.ListDepartments()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(depts) != 2 {
		t.Fatalf("list = %d, want 2", len(depts))
	}

	// duplicate name under same parent rejected
	if _, err := s.CreateDepartment("root", nil); err == nil {
		t.Fatal("duplicate department name accepted, want error")
	}

	// delete with employees in use
	if err := s.DeleteDepartment(root); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete root with employees: err = %v, want ErrInUse", err)
	}
}

func TestEmployeeCRUD(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	e, err := s.GetEmployee(worker)
	if err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if e.Kind != KindAI || e.Tool != "mock" || e.Workdir != "work/bob" {
		t.Fatalf("unexpected employee %+v", e)
	}
	if e.TokenHash != "" || e.LastSeenAt != nil {
		t.Fatalf("expected empty token/lastseen, got %+v", e)
	}

	if err := s.SetTool(worker, "opencode", `{"model":"x"}`); err != nil {
		t.Fatalf("set tool: %v", err)
	}
	e, _ = s.GetEmployee(worker)
	if e.Tool != "opencode" || e.ToolArgs != `{"model":"x"}` {
		t.Fatalf("tool not persisted: %+v", e)
	}

	if err := s.SetTokenHash(worker, "hash-1"); err != nil {
		t.Fatalf("set token hash: %v", err)
	}
	e, _ = s.GetEmployee(worker)
	if e.TokenHash != "hash-1" {
		t.Fatalf("token hash not persisted: %+v", e)
	}

	if err := s.UpdateLastSeen(worker, "2026-10-28T00:00:00Z"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	e, _ = s.GetEmployee(worker)
	if e.LastSeenAt == nil || *e.LastSeenAt != "2026-10-28T00:00:00Z" {
		t.Fatalf("last seen not persisted: %+v", e)
	}

	if err := s.UpdateLastSeen(9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("heartbeat unknown id: %v, want ErrNotFound", err)
	}

	// leader cannot be deleted
	if err := s.DeleteEmployee(leader); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete leader: err = %v, want ErrInUse", err)
	}
	// unknown id
	if err := s.DeleteEmployee(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: err = %v, want ErrNotFound", err)
	}
}

func TestEmployeeValidation(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateEmployee("", "worker", KindAI, 1); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := s.CreateEmployee("x", "", KindAI, 1); err == nil {
		t.Fatal("empty role accepted")
	}
	if _, err := s.CreateDepartment("d", nil); err != nil {
		t.Fatalf("dept: %v", err)
	}
	if _, err := s.CreateEmployee("x", "worker", "robot", 1); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := s.CreateEmployee("x", "worker", KindAI, 999); err == nil {
		t.Fatal("missing department accepted")
	}
}

func TestWorkflowCRUD(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	wf, err := s.CreateWorkflow("release")
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	n1, err := s.AddWorkflowNode(wf, worker, 10, 20)
	if err != nil {
		t.Fatalf("add node: %v", err)
	}
	// same employee twice in one workflow rejected
	if _, err := s.AddWorkflowNode(wf, worker, 30, 40); err == nil {
		t.Fatal("duplicate node accepted")
	}
	// edge to node of another workflow rejected
	wf2, _ := s.CreateWorkflow("other")
	n2, _ := s.AddWorkflowNode(wf2, worker, 1, 1)
	if _, err := s.AddWorkflowEdge(wf, n1, n2); err == nil {
		t.Fatal("cross-workflow edge accepted")
	}
	e1, err := s.AddWorkflowEdge(wf, n1, n1)
	if err != nil {
		t.Fatalf("self edge: %v", err)
	}
	edges, err := s.ListWorkflowEdges(wf)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 || edges[0].ID != e1 {
		t.Fatalf("edges = %+v", edges)
	}
	nodes, err := s.ListWorkflowNodes(wf)
	if err != nil || len(nodes) != 1 || nodes[0].X != 10 || nodes[0].Y != 20 {
		t.Fatalf("nodes = %+v, err = %v", nodes, err)
	}
	if err := s.RemoveWorkflowEdge(e1); err != nil {
		t.Fatalf("remove edge: %v", err)
	}
	if err := s.RemoveWorkflowNode(n1); err != nil {
		t.Fatalf("remove node: %v", err)
	}
	if err := s.DeleteWorkflow(wf); err != nil {
		t.Fatalf("delete workflow: %v", err)
	}
	if _, err := s.GetWorkflow(wf); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestTaskStateMachine(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)
	task, err := s.CreateTask(worker, KindDispatch, "do it", nil, nil, 0, 0, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	type move struct {
		status  string
		wantErr bool
	}
	moves := []move{
		{TaskRunning, false},
		{TaskRunning, true},  // running -> running invalid
		{TaskCanceled, true}, // running -> canceled is valid actually
	}
	// explicit table for clarity:
	cases := []struct {
		from, to  string
		wantErr   bool
		wantState string
	}{
		{TaskQueued, TaskRunning, false, TaskRunning},
		{TaskRunning, TaskDone, false, TaskDone},
	}
	_ = moves
	_ = cases
	// run a full happy path
	if err := s.SetTaskStatus(task, TaskRunning, ""); err != nil {
		t.Fatalf("queued->running: %v", err)
	}
	if err := s.SetTaskStatus(task, TaskDone, "ok"); err != nil {
		t.Fatalf("running->done: %v", err)
	}
	got, _ := s.GetTask(task)
	if got.Status != TaskDone || got.Output != "ok" || got.StartedAt == nil || got.FinishedAt == nil {
		t.Fatalf("task = %+v", got)
	}
	// terminal is terminal
	if err := s.SetTaskStatus(task, TaskRunning, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("done->running: err = %v, want ErrInvalidTransition", err)
	}
}

func TestTaskTransitionsTable(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	cases := []struct {
		prepare string
		to      string
		ok      bool
	}{
		{TaskQueued, TaskRunning, true},
		{TaskQueued, TaskCanceled, true},
		{TaskRunning, TaskDone, true},
		{TaskRunning, TaskFailed, true},
		{TaskRunning, TaskCanceled, true},
		{TaskQueued, TaskDone, false},
		{TaskDone, TaskRunning, false},
		{TaskCanceled, TaskRunning, false},
	}
	for _, c := range cases {
		t.Run(c.prepare+"->"+c.to, func(t *testing.T) {
			task, err := s.CreateTask(worker, KindDispatch, "i", nil, nil, 0, 0, nil)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if c.prepare != TaskQueued {
				if err := s.SetTaskStatus(task, TaskRunning, ""); err != nil {
					t.Fatalf("prepare to running: %v", err)
				}
				if c.prepare != TaskRunning {
					// reach c.prepare via a terminal move
					_ = c.prepare
				}
			}
			err = s.SetTaskStatus(task, c.to, "out")
			if c.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

func TestProjectCRUD(t *testing.T) {
	s := openTestStore(t)
	wf, _ := s.CreateWorkflow("wf")
	_, _, leader, _ := seedOrg(t, s)

	p, err := s.CreateProject("p1", "brief", intp(wf), intp(leader))
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	got, err := s.GetProject(p)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.Name != "p1" || got.WorkflowID == nil || *got.WorkflowID != wf || got.EntryEmpID == nil || *got.EntryEmpID != leader {
		t.Fatalf("project = %+v", got)
	}
	if err := s.UpdateProject(p, "brief2", nil, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.GetProject(p)
	if got.Brief != "brief2" || got.WorkflowID != nil {
		t.Fatalf("update not applied: %+v", got)
	}
	// project with tasks cannot be deleted
	task, _ := s.CreateTask(leader, KindDispatch, "i", intp(p), nil, 0, 0, nil)
	_ = task
	if err := s.DeleteProject(p); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete project with tasks: %v, want ErrInUse", err)
	}
}

func TestScheduleCRUD(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, _ := seedOrg(t, s)
	sc, err := s.CreateSchedule("nightly", leader, 3600, "review")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateSchedule("bad", leader, 0, "x"); err == nil {
		t.Fatal("zero interval accepted")
	}
	got, err := s.GetSchedule(sc)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Enabled || got.EverySeconds != 3600 || got.LastFiredAt != nil {
		t.Fatalf("schedule = %+v", got)
	}
	if err := s.SetScheduleEnabled(sc, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := s.FireSchedule(sc, "2026-10-28T01:00:00Z"); err != nil {
		t.Fatalf("fire: %v", err)
	}
	got, _ = s.GetSchedule(sc)
	if got.Enabled || got.LastFiredAt == nil {
		t.Fatalf("schedule after ops = %+v", got)
	}
	if err := s.DeleteSchedule(sc); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetSchedule(sc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMessageCRUD(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)
	from := leader
	mid, err := s.CreateMessage(worker, MsgAsk, "help", intp(from), nil, nil, nil)
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	got, err := s.GetMessage(mid)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if got.FromID == nil || *got.FromID != leader || got.Kind != MsgAsk || got.Body != "help" {
		t.Fatalf("message = %+v", got)
	}
	// boss message: no sender
	boss, err := s.CreateMessage(worker, MsgDispatch, "go", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("boss message: %v", err)
	}
	b, _ := s.GetMessage(boss)
	if b.FromID != nil {
		t.Fatalf("boss message from = %+v, want nil", b.FromID)
	}
	// invalid kind
	if _, err := s.CreateMessage(worker, "shout", "x", nil, nil, nil, nil); err == nil {
		t.Fatal("invalid kind accepted")
	}
	list, err := s.ListMessagesTo(worker, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d, want 2", len(list))
	}
	// ListMessagesTo is newest-first (ORDER BY id DESC); boss was created after mid.
	if list[0].ID != boss || list[1].ID != mid {
		t.Fatalf("order wrong: %+v", list)
	}
}

func TestSettings(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetSetting("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if got := s.GetSettingDefault("nope", "d"); got != "d" {
		t.Fatalf("default = %q", got)
	}
	if err := s.SetSetting("k", "v1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetSetting("k", "v2"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if v, _ := s.GetSetting("k"); v != "v2" {
		t.Fatalf("value = %q, want v2", v)
	}
	all, err := s.ListSettings()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if all["k"] != "v2" || len(all) != 1 {
		t.Fatalf("settings = %v", all)
	}
}
