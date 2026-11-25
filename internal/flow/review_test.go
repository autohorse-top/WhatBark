package flow

import (
	"strconv"
	"strings"
	"testing"

	"whatbark/internal/store"
)

// reviewTasksOf lists review-kind tasks of emp, newest last.
func reviewTasksOf(t *testing.T, st *store.Store, emp int) []store.Task {
	t.Helper()
	list, err := st.ListTasksByEmployee(emp, nil)
	if err != nil {
		t.Fatalf("list tasks of %d: %v", emp, err)
	}
	var out []store.Task
	for _, tk := range list {
		if tk.Kind == store.KindReview {
			out = append(out, tk)
		}
	}
	return out
}

// reviewMsgsTo lists review-kind messages addressed to emp.
func reviewMsgsTo(t *testing.T, st *store.Store, emp int) []store.Message {
	t.Helper()
	list, err := st.ListMessagesTo(emp, 50)
	if err != nil {
		t.Fatalf("list messages to %d: %v", emp, err)
	}
	var out []store.Message
	for _, m := range list {
		if m.Kind == store.MsgReview {
			out = append(out, m)
		}
	}
	return out
}

// msgKind returns messages of the given kind addressed to emp.
func msgKind(t *testing.T, st *store.Store, emp int, kind string) []store.Message {
	t.Helper()
	list, err := st.ListMessagesTo(emp, 50)
	if err != nil {
		t.Fatalf("list messages to %d: %v", emp, err)
	}
	var out []store.Message
	for _, m := range list {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

// dispatchAndFinish sends a dispatch L2->W1, moves it to running, and
// finishes it — the minimal wake-triggering sequence.
func dispatchAndFinish(t *testing.T, fx *struct {
	st   *store.Store
	flow *Flow
	L1   int
	L2   int
	W1   int
}, ok bool, output string) (taskID int) {
	t.Helper()
	_, taskID, err := fx.flow.Send(fx.L2, fx.W1, store.KindDispatch, "work item", nil, nil, nil, 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
		t.Fatalf("start task %d: %v", taskID, err)
	}
	if err := fx.flow.FinishTask(taskID, output, ok); err != nil {
		t.Fatalf("finish task %d: %v", taskID, err)
	}
	return taskID
}

// TestReviewWakeDispatchDone (F1.1): a dispatch finishing done wakes the AI
// requester with exactly one review task (kind/employee/requested_by/
// priority/nil attachments per F7) plus one review message (from_id NULL,
// linked to the review task). The normal report is unaffected.
func TestReviewWakeDispatchDone(t *testing.T) {
	fx := newFixtures(t)
	taskID := dispatchAndFinish(t, fx, true, "all done here")

	revs := reviewTasksOf(t, fx.st, fx.L2)
	if len(revs) != 1 {
		t.Fatalf("review tasks for requester = %d, want 1", len(revs))
	}
	rev := revs[0]
	if rev.RequestedBy != 0 || rev.Priority != 0 {
		t.Errorf("review task requested_by=%d priority=%d, want 0/0", rev.RequestedBy, rev.Priority)
	}
	if rev.ProjectID != nil || rev.WorkflowID != nil || rev.GroupID != nil {
		t.Errorf("review task has attachments: %+v", rev)
	}
	if rev.Status != store.TaskQueued {
		t.Errorf("review task status = %s, want queued", rev.Status)
	}
	wantHead := "Review wake: task #" + strconv.Itoa(taskID) + " by W1 is done."
	if !strings.HasPrefix(rev.Input, wantHead) {
		t.Errorf("review input head = %q, want prefix %q", rev.Input, wantHead)
	}

	rmsgs := reviewMsgsTo(t, fx.st, fx.L2)
	if len(rmsgs) != 1 {
		t.Fatalf("review messages to requester = %d, want 1", len(rmsgs))
	}
	if rmsgs[0].FromID != nil {
		t.Errorf("review message from_id = %v, want NULL (boss/system form)", *rmsgs[0].FromID)
	}
	if rmsgs[0].ToID == nil || *rmsgs[0].ToID != fx.L2 {
		t.Fatalf("review message to = %v, want %d", rmsgs[0].ToID, fx.L2)
	}
	if rmsgs[0].Body != rev.Input {
		t.Errorf("review message body differs from task input")
	}
	if rmsgs[0].TaskID == nil || *rmsgs[0].TaskID != rev.ID {
		t.Errorf("review message task_id = %v, want %d (back-filled)", rmsgs[0].TaskID, rev.ID)
	}

	// The automatic report still lands in the requester's thread (fail-soft
	// wake must not disturb the main reply path).
	reports := msgKind(t, fx.st, fx.L2, store.MsgReport)
	if len(reports) != 1 || !strings.Contains(reports[0].Body, "all done here") {
		t.Errorf("reports to requester = %+v, want the done report", reports)
	}
}

// TestReviewWakeDispatchFailed (F1.2): a failed dispatch wakes the requester
// with the failed state in the body.
func TestReviewWakeDispatchFailed(t *testing.T) {
	fx := newFixtures(t)
	taskID := dispatchAndFinish(t, fx, false, "it exploded")

	revs := reviewTasksOf(t, fx.st, fx.L2)
	if len(revs) != 1 {
		t.Fatalf("review tasks for requester = %d, want 1", len(revs))
	}
	if want := "Review wake: task #" + strconv.Itoa(taskID) + " by W1 is failed."; !strings.Contains(revs[0].Input, want) {
		t.Errorf("review input = %q, want %q within", revs[0].Input, want)
	}
	if strings.Contains(revs[0].Input, "done") {
		t.Errorf("failed wake body must not carry the done state: %q", revs[0].Input)
	}
}

// TestReviewWakeOnlyForDispatch (F1.3): ask, scheduled, selfreview, and
// review tasks finishing never trigger a wake, even with an AI requester.
func TestReviewWakeOnlyForDispatch(t *testing.T) {
	tests := []struct {
		name string
		kind string
	}{
		{"ask", store.KindAsk},
		{"scheduled", store.KindScheduled},
		{"selfreview", store.KindSelfReview},
		{"review", store.KindReview},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixtures(t)
			taskID, err := fx.st.CreateTask(fx.W1, tc.kind, "synthetic task", nil, nil, fx.L2, 0, nil)
			if err != nil {
				t.Fatalf("create %s task: %v", tc.kind, err)
			}
			if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
				t.Fatal(err)
			}
			if err := fx.flow.FinishTask(taskID, "finished output", true); err != nil {
				t.Fatalf("finish %s task: %v", tc.kind, err)
			}
			if got := len(reviewTasksOf(t, fx.st, fx.L2)); got != 0 {
				t.Errorf("%s finish created %d review tasks, want 0", tc.kind, got)
			}
			if got := len(reviewMsgsTo(t, fx.st, fx.L2)); got != 0 {
				t.Errorf("%s finish created %d review messages, want 0", tc.kind, got)
			}
		})
	}
}

// TestReviewWakeCanceledNoWake (F1.4): the cancel path never wakes — it
// never passes through FinishTask (it drives SetTaskStatus directly, like
// the API's CancelTask handler).
func TestReviewWakeCanceledNoWake(t *testing.T) {
	fx := newFixtures(t)
	// Table: which running state the task is canceled from. The cancel
	// path drives SetTaskStatus directly (like the API's CancelTask
	// handler) and never passes through FinishTask.
	tests := []struct {
		name   string
		toStep func(id int)
	}{
		{"queued->canceled", func(id int) {}},
		{"running->canceled", func(id int) {
			if err := fx.st.SetTaskStatus(id, store.TaskRunning, ""); err != nil {
				t.Fatalf("start task %d: %v", id, err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, taskID, err := fx.flow.Send(fx.L2, fx.W1, store.KindDispatch, "doomed", nil, nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			tc.toStep(taskID)
			if err := fx.st.SetTaskStatus(taskID, store.TaskCanceled, "not needed"); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			if n := len(reviewTasksOf(t, fx.st, fx.L2)); n != 0 {
				t.Errorf("cancel woke %d reviews, want 0", n)
			}
			if n := len(reviewMsgsTo(t, fx.st, fx.L2)); n != 0 {
				t.Errorf("cancel woke %d review messages, want 0", n)
			}
		})
	}
}

// TestReviewWakeRequesterMatrix (F1.5): a boss requester (0) and a human
// requester are never woken, in either terminal state.
func TestReviewWakeRequesterMatrix(t *testing.T) {
	t.Run("boss requester", func(t *testing.T) {
		fx := newFixtures(t)
		_, taskID, err := fx.flow.Send(0, fx.W1, store.KindDispatch, "boss work", nil, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
			t.Fatal(err)
		}
		if err := fx.flow.FinishTask(taskID, "done", true); err != nil {
			t.Fatal(err)
		}
		for _, emp := range []int{fx.L1, fx.L2, fx.W1} {
			if n := len(reviewTasksOf(t, fx.st, emp)); n != 0 {
				t.Errorf("boss dispatch woke %d reviews for %d, want 0", n, emp)
			}
		}
	})

	t.Run("human requester", func(t *testing.T) {
		fx := newFixtures(t)
		// Promote a human employee to W1's department leader so the
		// dispatch route L2 would have is now legally hers.
		var deptB int
		depts, err := fx.st.ListDepartments()
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range depts {
			if d.Name == "B" {
				deptB = d.ID
			}
		}
		human, err := fx.st.CreateEmployee("HUMAN", "lead", store.KindHuman, deptB)
		if err != nil {
			t.Fatal(err)
		}
		if err := fx.st.SetDepartmentLeader(deptB, human); err != nil {
			t.Fatalf("set human leader: %v", err)
		}
		_, taskID, err := fx.flow.Send(human, fx.W1, store.KindDispatch, "human work", nil, nil, nil, 0)
		if err != nil {
			t.Fatalf("human->W1 dispatch: %v", err)
		}
		if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
			t.Fatal(err)
		}
		if err := fx.flow.FinishTask(taskID, "done", true); err != nil {
			t.Fatal(err)
		}
		if n := len(reviewTasksOf(t, fx.st, human)); n != 0 {
			t.Errorf("human requester woken with %d reviews, want 0", n)
		}
		if n := len(reviewMsgsTo(t, fx.st, human)); n != 0 {
			t.Errorf("human requester got %d review messages, want 0", n)
		}
	})
}

// TestReviewWakeThrottledAtLimit (F1.6): with 30 reviews already injected
// in the hour, the 31st dispatch finish skips the wake but keeps the
// FinishTask contract: no error, report intact, and an audit row records
// the skip.
func TestReviewWakeThrottledAtLimit(t *testing.T) {
	fx := newFixtures(t)
	for i := 0; i < 30; i++ {
		if _, err := fx.st.CreateTask(fx.L2, store.KindReview, "prior wake", nil, nil, 0, 0, nil); err != nil {
			t.Fatalf("seed review %d: %v", i, err)
		}
	}
	dispatchAndFinish(t, fx, true, "shipped anyway")

	// No new review beyond the 30 seeded ones.
	if got, err := fx.st.CountEmployeeReviewsInHour(fx.L2); err != nil || got != 30 {
		t.Fatalf("reviews in hour = %d (err %v), want 30", got, err)
	}
	// The report still made it (fail-soft).
	if reports := msgKind(t, fx.st, fx.L2, store.MsgReport); len(reports) != 1 {
		t.Fatalf("reports after throttle = %d, want 1", len(reports))
	}
	// The throttle skip is audited.
	audit, err := fx.st.AuditList(5)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	var sawThrottled bool
	for _, a := range audit {
		if a.Action == "review.skip.throttled" {
			sawThrottled = true
			if a.ActorID != fx.L2 || !strings.Contains(a.Detail, "limit=30") {
				t.Errorf("throttle audit = %+v, want actor %d and limit=30", a, fx.L2)
			}
		}
	}
	if !sawThrottled {
		t.Errorf("no review.skip.throttled audit row: %+v", audit)
	}
}

// TestReviewWakePileupNoMerge (F1.7): three different dispatches finishing
// for the same requester produce three independent review tasks/messages —
// no deduplication, no merging.
func TestReviewWakePileupNoMerge(t *testing.T) {
	fx := newFixtures(t)
	for i := 0; i < 3; i++ {
		dispatchAndFinish(t, fx, true, "output number "+strconv.Itoa(i))
	}
	revs := reviewTasksOf(t, fx.st, fx.L2)
	if len(revs) != 3 {
		t.Fatalf("review tasks after 3 finishes = %d, want 3", len(revs))
	}
	seen := map[int]bool{}
	for _, rev := range revs {
		if seen[rev.ID] {
			t.Errorf("duplicate review task id %d", rev.ID)
		}
		seen[rev.ID] = true
	}
	rmsgs := reviewMsgsTo(t, fx.st, fx.L2)
	if len(rmsgs) != 3 {
		t.Fatalf("review messages after 3 finishes = %d, want 3", len(rmsgs))
	}
	linkIDs := map[int]bool{}
	for _, m := range rmsgs {
		if m.TaskID == nil || !seen[*m.TaskID] {
			t.Errorf("review message task_id = %v, want one of the review tasks", m.TaskID)
		} else {
			linkIDs[*m.TaskID] = true
		}
	}
	if len(linkIDs) != 3 {
		t.Errorf("distinct linked review tasks = %d, want 3", len(linkIDs))
	}
}

// TestReviewWakeBodyFiveElements (F1.8): the body carries the five required
// elements — subordinate name, task id, state word, UTF-8-safe input/output
// tails, and the guidance (re-dispatch cap + boss escalation) — in both
// done and failed variants.
func TestReviewWakeBodyFiveElements(t *testing.T) {
	fx := newFixtures(t)
	input := "HEADMARKER-XXX" + strings.Repeat("i", 600) + " TAILMARKER-ZZZ"
	output := "OUTHEAD-AAA" + strings.Repeat("o", 3000) + "-OUTTAIL"
	_, taskID, err := fx.flow.Send(fx.L2, fx.W1, store.KindDispatch, input, nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := fx.flow.FinishTask(taskID, output, true); err != nil {
		t.Fatal(err)
	}
	revs := reviewTasksOf(t, fx.st, fx.L2)
	if len(revs) != 1 {
		t.Fatalf("review tasks = %d, want 1", len(revs))
	}
	body := revs[0].Input

	tests := []struct {
		name    string
		substr  string
		present bool
	}{
		{"subordinate name", "by W1", true},
		{"task id", "task #" + strconv.Itoa(taskID), true},
		{"state word", "is done.", true},
		{"input tail kept", "TAILMARKER-ZZZ", true},
		{"input head dropped", "HEADMARKER-XXX", false},
		{"output tail kept", "-OUTTAIL", true},
		{"output head dropped", "OUTHEAD-AAA", false},
		{"guidance re-dispatch cap", "at most 2 more times", true},
		{"guidance boss escalation", "escalate to the boss", true},
	}
	for _, tc := range tests {
		if got := strings.Contains(body, tc.substr); got != tc.present {
			t.Errorf("%s: contains(%q) = %v, want %v (body=%q)", tc.name, tc.substr, got, tc.present, body)
		}
	}
	// Body is head-truncated: input tail <=512B + output tail <=2kB +
	// fixed template text (~300B) can never reach the 2900B mark.
	if n := len(revs[0].Input); n > 2900 {
		t.Errorf("review body length = %d, suspiciously untruncated", n)
	}
}

// TestTailUTF8: the byte-budget tail is exactly n or less, never splits a
// multi-byte rune, and handles the exact-boundary / empty / negative cases.
func TestTailUTF8(t *testing.T) {
	// "a世界b": a(1) 世(3) 界(3) b(1) = 8 bytes.
	const s = "a世界b"
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty string", "", 10, ""},
		{"shorter than budget", "abc", 10, "abc"},
		{"exact fit", "abcd", 4, "abcd"},
		{"exact fit on multibyte boundary", "世界", 3, "界"},
		{"ascii cut", "abcdef", 3, "def"},
		{"cut lands on rune start", s, 4, "界b"},
		{"cut lands mid rune", s, 3, "b"},
		{"cut lands mid rune, larger budget", s, 6, "界b"},
		{"almost whole string", s, 7, "世界b"},
		{"whole multibyte tail", "a世", 3, "世"},
		{"budget inside final multibyte", "a世", 2, ""},
		{"budget zero", "abc", 0, ""},
		{"budget negative", "abc", -2, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tailUTF8(tc.in, tc.n)
			if got != tc.want {
				t.Fatalf("tailUTF8(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			if tc.n > 0 && len(got) > tc.n {
				t.Errorf("result %q exceeds budget %d", got, tc.n)
			}
		})
	}
}

// TestReviewTaskFinishNoReceiptNoRewake (F7): completing a review task
// produces no automatic receipt and no second-generation wake. The review
// task itself must carry the system-only shape (requested_by=0, no group).
func TestReviewTaskFinishNoReceiptNoRewake(t *testing.T) {
	fx := newFixtures(t)
	// Simulate a previously injected wake (the system form: from=0).
	_, revTask, err := fx.flow.Send(0, fx.L2, store.KindReview, "Review wake: task #1 by W1 is done.", nil, nil, nil, 0)
	if err != nil {
		t.Fatalf("inject review: %v", err)
	}
	if err := fx.st.SetTaskStatus(revTask, store.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := fx.flow.FinishTask(revTask, "reviewed and accepted", true); err != nil {
		t.Fatal(err)
	}

	// No receipt: the requester thread still holds exactly the wake
	// message, with no report/summary/answer added.
	msgs, err := fx.st.ListMessagesTo(fx.L2, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Kind != store.MsgReview {
		t.Fatalf("messages to requester after review finish = %+v, want only the wake", msgs)
	}
	// Nobody else got a receipt either.
	for _, emp := range []int{fx.L1, fx.W1} {
		other, err := fx.st.ListMessagesTo(emp, 50)
		if err != nil {
			t.Fatal(err)
		}
		// W1 never sees anything of this flow; L1 neither.
		if len(other) != 0 {
			t.Errorf("employee %d received %+v, want none", emp, other)
		}
	}
	// No re-wake: the only review task is the finished one itself.
	if got, err := fx.st.CountEmployeeReviewsInHour(fx.L2); err != nil || got != 1 {
		t.Fatalf("reviews after review finish = %d (err %v), want 1", got, err)
	}
	rev, err := fx.st.GetTask(revTask)
	if err != nil || rev.Status != store.TaskDone || rev.RequestedBy != 0 {
		t.Fatalf("finished review = %+v (err %v)", rev, err)
	}
}

// TestReviewMaxPerHourClamp: the throttle read clamps hand-edited DB values
// into [0,1000] (S30 lesson) and falls back to the default 30 when the
// value is absent or broken.
func TestReviewMaxPerHourClamp(t *testing.T) {
	fx := newFixtures(t)
	tests := []struct {
		name  string
		set   string
		setIt bool
		want  int
		wake  bool // whether a dispatch finish then injects a review
	}{
		{"unset defaults to 30", "", false, 30, true},
		{"valid 42", "42", true, 42, true},
		{"boundary 1000", "1000", true, 1000, true},
		{"over the bound clamps to 1000", "1001", true, 1000, true},
		{"negative clamps to 0 (disabled)", "-5", true, 0, false},
		{"zero disables", "0", true, 0, false},
		{"junk falls back to default", "x", true, 30, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setIt {
				if err := fx.st.SetSetting("review_max_per_hour", tc.set); err != nil {
					t.Fatal(err)
				}
			}
			if got := fx.flow.reviewMaxPerHour(); got != tc.want {
				t.Fatalf("reviewMaxPerHour() = %d, want %d", got, tc.want)
			}
			// Behavioral check: does a dispatch finish wake?
			before := len(reviewTasksOf(t, fx.st, fx.L2))
			dispatchAndFinish(t, fx, true, "out")
			after := len(reviewTasksOf(t, fx.st, fx.L2))
			woke := after == before+1
			if woke != tc.wake {
				t.Fatalf("wake after finish = %v, want %v (before=%d after=%d)", woke, tc.wake, before, after)
			}
		})
	}
}
