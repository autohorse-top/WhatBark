package sched

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"whatbark/internal/flow"
	"whatbark/internal/store"
)

func intp(v int) *int { return &v }

// TestHumanDuration (S30, review Minor-1) pins the reminder body's
// duration wording: sub-minute values must read as seconds, not "0 minutes".
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "0 sec"},
		{1 * time.Second, "1 sec"},
		{59 * time.Second, "59 sec"},
		{time.Minute, "1 min"},
		{5 * time.Minute, "5 min"},
		{59 * time.Minute, "59 min"},
		{time.Hour, "1 h"},
		{2*time.Hour + 30*time.Minute, "2 h"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// slaTestRig builds a store with two AI employees in one department, a group
// (w=creator, v=member), and a scheduler with an injected clock.
type slaTestRig struct {
	st  *store.Store
	w   int // creator
	v   int // member
	gid int
	sc  *Scheduler
	now time.Time
}

func newSlaRig(t *testing.T) *slaTestRig {
	t.Helper()
	st, w := testStore(t)
	depts, err := st.ListDepartments()
	if err != nil {
		t.Fatalf("ListDepartments: %v", err)
	}
	v, err := st.CreateEmployee("V", "worker", store.KindAI, depts[0].ID)
	if err != nil {
		t.Fatalf("CreateEmployee V: %v", err)
	}
	gid, err := st.CreateGroup("sla-g", w, []int{v}, "group")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	sc := New(st, flow.New(st, nil, nil))
	// Anchor on the real clock: a task created "now" has created_at ≈ now,
	// so the fresh-task exclusion is deterministic against cutoff now-300s.
	now := time.Now()
	sc.Now = func() time.Time { return now }
	return &slaTestRig{st: st, w: w, v: v, gid: gid, sc: sc, now: now}
}

// backdateSLATask pins a task's created_at to ts (stored as UTC
// 'YYYY-MM-DD HH:MM:SS', matching the schema default format).
func (r *slaTestRig) backdateSLATask(t *testing.T, taskID int, ts time.Time) {
	t.Helper()
	if _, err := r.st.DB().Exec(
		`UPDATE tasks SET created_at = ? WHERE id = ?`,
		ts.UTC().Format("2006-01-02 15:04:05"), taskID); err != nil {
		t.Fatalf("backdate task %d: %v", taskID, err)
	}
}

// slaReminders counts system messages in the group that look like SLA
// reminders (the group-creation system message must not count).
func (r *slaTestRig) slaReminders(t *testing.T) []store.Message {
	t.Helper()
	msgs, err := r.st.GroupMessages(r.gid, 0)
	if err != nil {
		t.Fatalf("GroupMessages: %v", err)
	}
	var out []store.Message
	for _, m := range msgs {
		if m.Kind == store.MsgSystem && strings.Contains(m.Body, "⏰ Task #") {
			out = append(out, m)
		}
	}
	return out
}

// TestGroupSLAReminder (S30): an enabled group (sla=300s) gets exactly one
// system reminder per open task past the threshold; the task is marked
// reminded after a successful send; a second tick does not re-remind.
func TestGroupSLAReminder(t *testing.T) {
	r := newSlaRig(t)
	if err := r.st.SetGroupPolicy(context.Background(), r.gid, nil, intp(300)); err != nil {
		t.Fatalf("SetGroupPolicy: %v", err)
	}
	taskID, err := r.st.CreateTask(r.v, store.KindDispatch, "slow work", nil, nil, r.w, 0, &r.gid)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.backdateSLATask(t, taskID, r.now.Add(-10*time.Minute))

	// Before the tick: no reminder.
	if got := r.slaReminders(t); len(got) != 0 {
		t.Fatalf("before step: %d reminders, want 0", len(got))
	}
	if err := r.sc.Step(r.now); err != nil {
		t.Fatalf("Step: %v", err)
	}
	got := r.slaReminders(t)
	if len(got) != 1 {
		t.Fatalf("after step: %d reminders, want 1: %+v", len(got), got)
	}
	body := got[0].Body
	if !strings.Contains(body, fmt.Sprintf("⏰ Task #%d", taskID)) {
		t.Errorf("reminder body %q missing task id %d", body, taskID)
	}
	if !strings.Contains(body, "SLA threshold (5 min)") {
		t.Errorf("reminder body %q missing threshold 5 min", body)
	}
	if m, err := r.st.GetMessage(got[0].ID); err != nil ||
		m.GroupID == nil || *m.GroupID != r.gid {
		t.Errorf("reminder not a group message: %+v (err %v)", m, err)
	}
	var ts string
	if err := r.st.DB().QueryRow(
		`SELECT sla_reminded_at FROM tasks WHERE id = ?`, taskID).Scan(&ts); err != nil {
		t.Fatalf("read sla_reminded_at: %v", err)
	}
	if ts == "" {
		t.Error("sla_reminded_at not set after reminder")
	}

	// Second tick at the same instant: no duplicate reminder.
	if err := r.sc.Step(r.now); err != nil {
		t.Fatalf("Step (2nd): %v", err)
	}
	if got := r.slaReminders(t); len(got) != 1 {
		t.Fatalf("after 2nd step: %d reminders, want 1 (no duplicate)", len(got))
	}
}

// TestGroupSLANegativeCases (S30): no reminder when the group's SLA is
// disabled, the task is still fresh, or the task is no longer open.
func TestGroupSLANegativeCases(t *testing.T) {
	// Disabled group (sla=0): an overdue task never gets reminded.
	{
		r := newSlaRig(t)
		taskID, err := r.st.CreateTask(r.v, store.KindDispatch, "slow", nil, nil, r.w, 0, &r.gid)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.backdateSLATask(t, taskID, r.now.Add(-10*time.Minute))
		if err := r.sc.Step(r.now); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if got := r.slaReminders(t); len(got) != 0 {
			t.Fatalf("disabled group: %d reminders, want 0", len(got))
		}
	}

	// Enabled group, fresh task (created now): not due.
	{
		r := newSlaRig(t)
		if err := r.st.SetGroupPolicy(context.Background(), r.gid, nil, intp(300)); err != nil {
			t.Fatalf("SetGroupPolicy: %v", err)
		}
		if _, err := r.st.CreateTask(r.v, store.KindDispatch, "fresh", nil, nil, r.w, 0, &r.gid); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if err := r.sc.Step(r.now); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if got := r.slaReminders(t); len(got) != 0 {
			t.Fatalf("fresh task: %d reminders, want 0", len(got))
		}
	}

	// Enabled group, backdated task that finished: not open, no reminder.
	{
		r := newSlaRig(t)
		if err := r.st.SetGroupPolicy(context.Background(), r.gid, nil, intp(300)); err != nil {
			t.Fatalf("SetGroupPolicy: %v", err)
		}
		taskID, err := r.st.CreateTask(r.v, store.KindDispatch, "old done", nil, nil, r.w, 0, &r.gid)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if err := r.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := r.st.SetTaskStatus(taskID, store.TaskDone, "x"); err != nil {
			t.Fatalf("finish: %v", err)
		}
		r.backdateSLATask(t, taskID, r.now.Add(-10*time.Minute))
		if err := r.sc.Step(r.now); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if got := r.slaReminders(t); len(got) != 0 {
			t.Fatalf("done task: %d reminders, want 0", len(got))
		}
	}
}
