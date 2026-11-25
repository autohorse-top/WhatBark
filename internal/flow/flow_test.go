package flow

import (
	"errors"
	"strings"
	"testing"

	"whatbark/internal/store"
)

// newFixtures builds a small org:
//
//	dept A (leader L1)
//	dept B (parent A, leader L2)
//	L1 in A, L2 and W1 in B.
//
// Up[W1] = [L2, L1]; Up[L2] = [L1]; Up[L1] = [].
func newFixtures(t *testing.T) *struct {
	st   *store.Store
	flow *Flow
	L1   int
	L2   int
	W1   int
} {
	t.Helper()
	db := t.TempDir() + "/flow.db"
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	deptA, err := st.CreateDepartment("A", nil)
	if err != nil {
		t.Fatal(err)
	}
	deptB, err := st.CreateDepartment("B", intp(deptA))
	if err != nil {
		t.Fatal(err)
	}
	l1, err := st.CreateEmployee("L1", "lead", store.KindAI, deptA)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := st.CreateEmployee("L2", "lead", store.KindAI, deptB)
	if err != nil {
		t.Fatal(err)
	}
	w1, err := st.CreateEmployee("W1", "worker", store.KindAI, deptB)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDepartmentLeader(deptA, l1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDepartmentLeader(deptB, l2); err != nil {
		t.Fatal(err)
	}
	f := New(st, nil, nil)
	return &struct {
		st   *store.Store
		flow *Flow
		L1   int
		L2   int
		W1   int
	}{st: st, flow: f, L1: l1, L2: l2, W1: w1}
}

func TestNewGraphTopology(t *testing.T) {
	fx := newFixtures(t)
	g, err := NewGraph(fx.st)
	if err != nil {
		t.Fatal(err)
	}
	if !g.IsLeader[fx.L1] || !g.IsLeader[fx.L2] {
		t.Error("leaders not marked")
	}
	if len(g.Up[fx.W1]) != 2 || g.Up[fx.W1][0] != fx.L2 || g.Up[fx.W1][1] != fx.L1 {
		t.Errorf("Up[W1] = %v, want [L2 L1]", g.Up[fx.W1])
	}
	if !g.Down[fx.L1][fx.W1] || !g.Down[fx.L1][fx.L2] {
		t.Errorf("Down[L1] = %v, want {L2 W1}", g.Down[fx.L1])
	}
}

func TestSendDispatchCreatesTask(t *testing.T) {
	fx := newFixtures(t)
	msgID, taskID, err := fx.flow.Send(fx.L2, fx.W1, store.KindDispatch, "do the thing", nil, nil, nil, 0)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if msgID == 0 || taskID == 0 {
		t.Fatalf("ids = %d,%d", msgID, taskID)
	}
	task, err := fx.st.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Kind != store.KindDispatch || task.Status != store.TaskQueued || task.RequestedBy != fx.L2 {
		t.Errorf("task = %+v", task)
	}
	msgs, err := fx.st.ListMessagesTo(fx.W1, 10)
	if err != nil || len(msgs) != 1 || msgs[0].Kind != store.MsgDispatch {
		t.Errorf("messages = %+v err=%v", msgs, err)
	}
}

func TestSendRouteEnforced(t *testing.T) {
	fx := newFixtures(t)
	if _, _, err := fx.flow.Send(fx.W1, fx.L2, store.KindDispatch, "nope", nil, nil, nil, 0); err == nil {
		t.Fatal("worker dispatching to leader should be rejected")
	}
	if _, _, err := fx.flow.Send(0, fx.W1, "shout", "nope", nil, nil, nil, 0); err == nil {
		t.Fatal("unknown kind should be rejected")
	}
	if _, _, err := fx.flow.Send(fx.L2, fx.L2, store.MsgChat, "nope", nil, nil, nil, 0); err == nil {
		t.Fatal("self-send should be rejected")
	}
	if _, taskID, err := fx.flow.Send(fx.W1, fx.L2, store.MsgReport, "all done", nil, nil, nil, 0); err != nil || taskID != 0 {
		t.Fatalf("report send: ids=%d err=%v", taskID, err)
	}
}

func TestSeedProjectSeedsEntryTask(t *testing.T) {
	fx := newFixtures(t)
	pid, err := fx.flow.SeedProject("P", "build the cat", nil, intp(fx.L2))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := fx.st.ListTasksByEmployee(fx.L2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Kind != store.KindDispatch || tasks[0].RequestedBy != 0 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if tasks[0].ProjectID == nil || *tasks[0].ProjectID != pid {
		t.Error("task not bound to project")
	}
}

// TestFinishAutoReplies verifies the §7.5 automatic replies, using
// fresh fixtures per sub-case to avoid message cross-contamination.
func TestFinishAutoReplies(t *testing.T) {
	t.Run("ask_answer", func(t *testing.T) {
		fx := newFixtures(t)
		_, taskID, err := fx.flow.Send(fx.L2, fx.W1, store.KindAsk, "quick question", nil, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
			t.Fatal(err)
		}
		if err := fx.flow.FinishTask(taskID, "here is the answer", true); err != nil {
			t.Fatal(err)
		}
		msgs, _ := fx.st.ListMessagesTo(fx.L2, 10)
		if len(msgs) != 1 || msgs[0].Kind != store.MsgAnswer || !strings.Contains(msgs[0].Body, "here is the answer") {
			t.Errorf("L2 inbox after ask/answer = %+v", msgs)
		}
	})

	t.Run("boss_dispatch_report_to_entry", func(t *testing.T) {
		fx := newFixtures(t)
		pid, err := fx.st.CreateProject("P2", "", nil, intp(fx.L1))
		if err != nil {
			t.Fatal(err)
		}
		_, taskID, err := fx.flow.Send(0, fx.W1, store.KindDispatch, "work item", intp(pid), nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
			t.Fatal(err)
		}
		if err := fx.flow.FinishTask(taskID, "shipped it", true); err != nil {
			t.Fatal(err)
		}
		msgs, _ := fx.st.ListMessagesTo(fx.L1, 10)
		if len(msgs) != 1 || msgs[0].Kind != store.MsgReport || !strings.Contains(msgs[0].Body, "shipped it") {
			t.Errorf("L1 inbox after boss-dispatch/report = %+v", msgs)
		}
	})

	t.Run("selfreview_summary_to_first_leader", func(t *testing.T) {
		fx := newFixtures(t)
		taskID, err := fx.st.CreateTask(fx.W1, store.KindSelfReview, "review yourself", nil, nil, 0, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
			t.Fatal(err)
		}
		if err := fx.flow.FinishTask(taskID, "all quiet", true); err != nil {
			t.Fatal(err)
		}
		msgs, _ := fx.st.ListMessagesTo(fx.L2, 10)
		if len(msgs) != 1 || msgs[0].Kind != store.MsgSummary || !strings.Contains(msgs[0].Body, "all quiet") {
			t.Errorf("L2 inbox after selfreview = %+v", msgs)
		}
	})

	t.Run("no_target_no_message", func(t *testing.T) {
		fx := newFixtures(t)
		_, taskID, err := fx.flow.Send(0, fx.W1, store.KindScheduled, "periodic", nil, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
			t.Fatal(err)
		}
		if err := fx.flow.FinishTask(taskID, "tick", true); err != nil {
			t.Fatal(err)
		}
		msgs1, _ := fx.st.ListMessagesTo(fx.L1, 10)
		msgs2, _ := fx.st.ListMessagesTo(fx.L2, 10)
		if len(msgs1) != 0 {
			t.Errorf("L1 should have 0 messages, got %+v", msgs1)
		}
		if len(msgs2) != 0 {
			t.Errorf("L2 should have 0 messages, got %+v", msgs2)
		}
	})
}

func TestFinishFailedMarksFailed(t *testing.T) {
	fx := newFixtures(t)
	_, taskID, err := fx.flow.Send(fx.L2, fx.W1, store.KindAsk, "q", nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := fx.flow.FinishTask(taskID, "boom", false); err != nil {
		t.Fatal(err)
	}
	task, _ := fx.st.GetTask(taskID)
	if task.Status != store.TaskFailed {
		t.Errorf("status = %q, want failed", task.Status)
	}
}

// TestDispatchToGroup (S22): a dispatch inside a group thread creates a group
// message (kind=dispatch, group_id set, to_id NULL, task_id back-filled) and
// a task bound to the group; FinishTask then routes the report back into the
// group thread instead of the 1:1 inbox.
func TestDispatchToGroup(t *testing.T) {
	fx := newFixtures(t)
	gid, err := fx.st.CreateGroup("dispatch-group", fx.L2, []int{fx.W1}, "")
	if err != nil {
		t.Fatal(err)
	}

	msgID, taskID, err := fx.flow.DispatchToGroup(fx.L2, gid, fx.W1, "group task", nil, nil, nil)
	if err != nil {
		t.Fatalf("DispatchToGroup: %v", err)
	}
	if msgID == 0 || taskID == 0 {
		t.Fatalf("ids = %d,%d", msgID, taskID)
	}
	msg, err := fx.st.GetMessage(msgID)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Kind != store.MsgDispatch || msg.GroupID == nil || *msg.GroupID != gid || msg.ToID != nil {
		t.Errorf("group dispatch message = %+v, want kind=dispatch group=%d to=NULL", msg, gid)
	}
	if msg.TaskID == nil || *msg.TaskID != taskID {
		t.Errorf("message task_id = %v, want %d", msg.TaskID, taskID)
	}
	task, err := fx.st.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.EmpID != fx.W1 || task.RequestedBy != fx.L2 || task.GroupID == nil || *task.GroupID != gid {
		t.Errorf("task = %+v, want W1 / L2 / group %d", task, gid)
	}

	// W1 finishes: the report must land in the group thread, not L2's 1:1 inbox.
	if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := fx.flow.FinishTask(taskID, "group done", true); err != nil {
		t.Fatal(err)
	}
	gmsgs, err := fx.st.GroupMessages(gid, 0)
	if err != nil {
		t.Fatal(err)
	}
	var report *store.Message
	for i := range gmsgs {
		if gmsgs[i].Kind == store.MsgReport {
			report = &gmsgs[i]
		}
	}
	if report == nil || !strings.Contains(report.Body, "group done") {
		t.Errorf("group messages after finish = %+v, want a report", gmsgs)
	}
	if report != nil && (report.TaskID == nil || *report.TaskID != taskID) {
		t.Errorf("report task_id = %v, want %d", report.TaskID, taskID)
	}
	l2msgs, _ := fx.st.ListMessagesTo(fx.L2, 10)
	// S1 review wake: the group-dispatched finish also wakes requester L2
	// with a 1:1 review (TASK §3.1: group dispatches wake like 1:1 ones; the
	// review task itself never joins the group). What must stay true is the
	// S22 invariant: no *report* leaks into the 1:1 inbox — the report went
	// to the group thread above.
	revTasks := 0
	for _, m := range l2msgs {
		if m.Kind == store.MsgReview {
			revTasks++
			continue
		}
		t.Errorf("L2 1:1 inbox after group report = %+v, want only the review wake (no report)", m)
	}
	if revTasks != 1 {
		t.Errorf("group-dispatch finish woke %d reviews, want 1", revTasks)
	}
}

// TestDispatchToGroupRejections (S22): permission matrix. 403-grade errors
// (sender not a member, route not allowed) wrap ErrRouteNotAllowed; plain
// errors (no target, self, target not a member, empty body) must not.
func TestDispatchToGroupRejections(t *testing.T) {
	fx := newFixtures(t)
	gid, err := fx.st.CreateGroup("perm-group", fx.L1, []int{fx.L2}, "")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		from, to  int
		body      string
		wantRoute bool
	}{
		{"no target", fx.L2, 0, "x", false},
		{"self", fx.L2, fx.L2, "x", false},
		{"sender not member", fx.W1, fx.L2, "x", true},
		{"target not member", fx.L1, fx.W1, "x", false},
		{"lateral route", fx.L2, fx.L1, "x", true},
		{"empty body", fx.L1, fx.L2, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := fx.flow.DispatchToGroup(tc.from, gid, tc.to, tc.body, nil, nil, nil)
			if err == nil {
				t.Fatal("DispatchToGroup accepted, want error")
			}
			if got := errors.Is(err, ErrRouteNotAllowed); got != tc.wantRoute {
				t.Errorf("err = %v, wantRouteNotAllowed=%v", err, tc.wantRoute)
			}
		})
	}
}

// TestGroupReportFallsBackOnMemberLoss (S22): when the assignee leaves the
// group before finishing, the report falls back to the 1:1 requester.
func TestGroupReportFallsBackOnMemberLoss(t *testing.T) {
	fx := newFixtures(t)
	gid, err := fx.st.CreateGroup("loss-group", fx.L2, []int{fx.W1}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, taskID, err := fx.flow.DispatchToGroup(fx.L2, gid, fx.W1, "task", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	// Simulate W1 leaving the group (no store-level removal API).
	if _, err := fx.st.DB().Exec(
		`DELETE FROM group_members WHERE group_id = ? AND employee_id = ?`, gid, fx.W1); err != nil {
		t.Fatal(err)
	}
	if err := fx.flow.FinishTask(taskID, "left but finished", true); err != nil {
		t.Fatal(err)
	}
	msgs, _ := fx.st.ListMessagesTo(fx.L2, 10)
	// S1: besides the S22 fallback report, the finish wakes a 1:1 review
	// (TASK §3.1) — filter by kind so the S22 invariant stays pinned: the
	// member-loss fallback still produces exactly one 1:1 report.
	var reports []store.Message
	for _, m := range msgs {
		if m.Kind == store.MsgReport {
			reports = append(reports, m)
		}
	}
	if len(reports) != 1 || !strings.Contains(reports[0].Body, "left but finished") {
		t.Errorf("L2 1:1 inbox after member-loss fallback = %+v, want the report (plus the review wake)", msgs)
	}
}

// TestDispatchToGroupProject (S29): a dispatch carrying an existing project
// binds the created task to it (GetTask.ProjectID == pid); an unknown project
// id fails with a plain error (not ErrRouteNotAllowed) before any group
// message is created.
func TestDispatchToGroupProject(t *testing.T) {
	fx := newFixtures(t)
	gid, err := fx.st.CreateGroup("proj-group", fx.L2, []int{fx.W1}, "")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := fx.st.CreateProject("S29", "brief", nil, nil)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Existing project: task binds to it.
	msgID, taskID, err := fx.flow.DispatchToGroup(fx.L2, gid, fx.W1, "proj task", nil, nil, &pid)
	if err != nil {
		t.Fatalf("DispatchToGroup with project: %v", err)
	}
	if msgID == 0 || taskID == 0 {
		t.Fatalf("ids = %d,%d", msgID, taskID)
	}
	task, err := fx.st.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ProjectID == nil || *task.ProjectID != pid {
		t.Fatalf("task.ProjectID = %v, want %d", task.ProjectID, pid)
	}
	if task.GroupID == nil || *task.GroupID != gid {
		t.Fatalf("task.GroupID = %v, want %d", task.GroupID, gid)
	}

	// Unknown project: plain error (not route-not-allowed), no orphan message.
	unknown := 99999
	before, err := fx.st.GroupMessages(gid, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = fx.flow.DispatchToGroup(fx.L2, gid, fx.W1, "bad proj", nil, nil, &unknown)
	if err == nil {
		t.Fatal("DispatchToGroup with unknown project accepted, want error")
	}
	if errors.Is(err, ErrRouteNotAllowed) {
		t.Fatalf("err = %v, must not wrap ErrRouteNotAllowed", err)
	}
	after, err := fx.st.GroupMessages(gid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("group messages after failed dispatch = %d, want %d (no orphan message)", len(after), len(before))
	}
}
