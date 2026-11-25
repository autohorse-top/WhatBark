package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestReassignTask (S27) verifies the reassign state machine: any of
// queued/running/canceled/failed can be reassigned to a new owner and reset
// to queued; done is terminal; unknown task/employee produce typed errors.
func TestReassignTask(t *testing.T) {
	s := openTestStore(t)
	root, _, _, worker := seedOrg(t, s)
	other, err := s.CreateEmployee("carol", "worker", KindAI, root)
	if err != nil {
		t.Fatalf("create target employee: %v", err)
	}

	mk := func(status string) int {
		t.Helper()
		id, err := s.CreateTask(worker, KindDispatch, "input", nil, nil, 0, 0, nil)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		if status != TaskQueued {
			if err := s.SetTaskStatus(id, TaskRunning, ""); err != nil {
				t.Fatalf("prepare running: %v", err)
			}
			if status != TaskRunning {
				if err := s.SetTaskStatus(id, status, "previous"); err != nil {
					t.Fatalf("prepare %s: %v", status, err)
				}
			}
		}
		return id
	}

	cases := []struct {
		name       string
		from       string
		wantErr    error
		wantStatus string
		wantEmp    int
	}{
		{"queued", TaskQueued, nil, TaskQueued, other},
		{"running", TaskRunning, nil, TaskQueued, other},
		{"canceled", TaskCanceled, nil, TaskQueued, other},
		{"failed", TaskFailed, nil, TaskQueued, other},
		{"done", TaskDone, ErrInvalidTransition, TaskDone, worker},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := mk(c.from)
			err := s.ReassignTask(id, other, worker, "reassigned: bob -> carol")
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("ReassignTask: err = %v, want %v", err, c.wantErr)
				}
				got, gerr := s.GetTask(id)
				if gerr != nil {
					t.Fatalf("get task: %v", gerr)
				}
				if got.Status != c.wantStatus || got.EmpID != c.wantEmp {
					t.Errorf("task mutated on rejected reassign: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReassignTask: %v", err)
			}
			got, gerr := s.GetTask(id)
			if gerr != nil {
				t.Fatalf("get task: %v", gerr)
			}
			if got.Status != c.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, c.wantStatus)
			}
			if got.EmpID != c.wantEmp {
				t.Errorf("owner = %d, want %d", got.EmpID, c.wantEmp)
			}
			if got.StartedAt != nil {
				t.Errorf("started_at = %q, want NULL", *got.StartedAt)
			}
			if got.FinishedAt != nil {
				t.Errorf("finished_at = %q, want NULL", *got.FinishedAt)
			}
			if !strings.Contains(got.Output, "reassigned: bob -> carol") {
				t.Errorf("output = %q, want reassign audit note", got.Output)
			}
			// cancelled/failed tasks carried prior output; verify it survives.
			if c.from == TaskCanceled || c.from == TaskFailed {
				if !strings.Contains(got.Output, "previous") {
					t.Errorf("output = %q, prior output lost", got.Output)
				}
			}
		})
	}

	// Unknown task -> ErrNotFound.
	if err := s.ReassignTask(999999, other, worker, "note"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown task: err = %v, want ErrNotFound", err)
	}
	// Unknown employee -> wrapped ErrNotFound, task untouched.
	id := mk(TaskQueued)
	if err := s.ReassignTask(id, 999999, worker, "note"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown employee: err = %v, want ErrNotFound", err)
	}
	if got, _ := s.GetTask(id); got.EmpID != worker {
		t.Errorf("owner = %d, want %d (task must be untouched)", got.EmpID, worker)
	}
}

// TestReassignTaskOwnerPin (S27 review R2): the UPDATE pins the owner the
// caller read (expectedOwner), closing the TOCTOU window where a concurrent
// reassign swaps the owner between the caller's permission read and its
// write. Both the sequential stale-read case and the true concurrent race
// must admit exactly the caller whose owner read is still valid.
func TestReassignTaskOwnerPin(t *testing.T) {
	s := openTestStore(t)
	root, _, _, worker := seedOrg(t, s)
	other, err := s.CreateEmployee("carol", "worker", KindAI, root)
	if err != nil {
		t.Fatalf("create target employee: %v", err)
	}
	dave, err := s.CreateEmployee("dave", "worker", KindAI, root)
	if err != nil {
		t.Fatalf("create second target employee: %v", err)
	}

	t.Run("stale expected owner rejected", func(t *testing.T) {
		id, err := s.CreateTask(worker, KindDispatch, "input", nil, nil, 0, 0, nil)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		// First hop succeeds: bob -> carol.
		if err := s.ReassignTask(id, other, worker, "reassigned: bob -> carol"); err != nil {
			t.Fatalf("first reassign: %v", err)
		}
		// Simulated stale read: a caller that still believes the owner is
		// bob (worker) must be rejected even though the task is queued
		// (a reassignable state) — the owner no longer matches its read.
		if err := s.ReassignTask(id, dave, worker, "reassigned: bob -> dave"); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("stale-owner reassign: err = %v, want ErrInvalidTransition", err)
		}
		got, err := s.GetTask(id)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.EmpID != other {
			t.Errorf("owner = %d, want %d (stale call must not move the task)", got.EmpID, other)
		}
		if got.Status != TaskQueued {
			t.Errorf("status = %q, want %q", got.Status, TaskQueued)
		}
		// The fresh read (owner = carol) still works.
		if err := s.ReassignTask(id, dave, other, "reassigned: carol -> dave"); err != nil {
			t.Errorf("fresh-owner reassign: %v", err)
		}
		if got, _ := s.GetTask(id); got.EmpID != dave {
			t.Errorf("owner = %d, want %d", got.EmpID, dave)
		}
	})

	t.Run("concurrent reassigns one winner", func(t *testing.T) {
		id, err := s.CreateTask(worker, KindDispatch, "input", nil, nil, 0, 0, nil)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		// Both callers read owner=bob (worker) and race to move the task.
		const n = 4
		targets := []int{other, other, dave, dave}
		var wg sync.WaitGroup
		var okCount int32
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(target int) {
				defer wg.Done()
				if err := s.ReassignTask(id, target, worker, fmt.Sprintf("race to %d", target)); err == nil {
					atomic.AddInt32(&okCount, 1)
				}
			}(targets[i])
		}
		wg.Wait()
		// The owner-pinned UPDATE admits exactly one writer; the rest see the
		// moved owner and fail with ErrInvalidTransition (or a DB busy error
		// is also acceptable as a rejection — but the task must end at exactly
		// one target).
		got, err := s.GetTask(id)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if (got.EmpID != other) && (got.EmpID != dave) {
			t.Fatalf("owner = %d, want %d or %d", got.EmpID, other, dave)
		}
		if got.Status != TaskQueued {
			t.Errorf("status = %q, want %q", got.Status, TaskQueued)
		}
		// Count how many tasks landed on each target to infer winners: with a
		// single row, at most one distinct winner; okCount may be >1 only if
		// multiple racers wrote the same target before the pin moved (the pin
		// moves on the first write, so okCount must be exactly 1).
		if got := atomic.LoadInt32(&okCount); got != 1 {
			t.Errorf("winners = %d, want exactly 1 (owner-pinned UPDATE must admit a single writer)", got)
		}
	})
}

// TestSetTaskStatusConcurrent (S27 M2): concurrent running->done and
// running->canceled on the same task must produce exactly one winner.
// The pre-fix check-then-act allowed both callers to pass the allowedMoves
// precheck and silently overwrite each other.
func TestSetTaskStatusConcurrent(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	// --- Subtest A: single task, 10 racers (5 done + 5 canceled) ---
	// Exactly one transition may win; the other nine must be rejected.
	t.Run("single task ten racers", func(t *testing.T) {
		task, err := s.CreateTask(worker, KindDispatch, "i", nil, nil, 0, 0, nil)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		if err := s.SetTaskStatus(task, TaskRunning, ""); err != nil {
			t.Fatalf("start task: %v", err)
		}

		const n = 10
		var wg sync.WaitGroup
		var okCount, rejected, unexpected int32
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				var err error
				if i%2 == 0 {
					err = s.SetTaskStatus(task, TaskDone, "done")
				} else {
					err = s.SetTaskStatus(task, TaskCanceled, "cancel")
				}
				switch {
				case err == nil:
					atomic.AddInt32(&okCount, 1)
				case errors.Is(err, ErrInvalidTransition):
					atomic.AddInt32(&rejected, 1)
				default:
					atomic.AddInt32(&unexpected, 1)
				}
			}(i)
		}
		wg.Wait()

		if got := atomic.LoadInt32(&okCount); got != 1 {
			t.Errorf("winners = %d, want exactly 1", got)
		}
		if got := atomic.LoadInt32(&rejected); got != n-1 {
			t.Errorf("rejected = %d, want %d", got, n-1)
		}
		if got := atomic.LoadInt32(&unexpected); got != 0 {
			t.Errorf("unexpected errors = %d, want 0", got)
		}
		got, err := s.GetTask(task)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		switch got.Status {
		case TaskDone:
			if got.Output != "done" {
				t.Errorf("output = %q, want %q", got.Output, "done")
			}
		case TaskCanceled:
			if got.Output != "cancel" {
				t.Errorf("output = %q, want %q", got.Output, "cancel")
			}
		default:
			t.Errorf("final status = %q, want done|canceled", got.Status)
		}
	})

	// --- Subtest B: five tasks, each raced by one done + one canceled ---
	// Yields exactly 5 successes and 5 ErrInvalidTransition (spec 5/5).
	t.Run("five tasks", func(t *testing.T) {
		const n = 5
		ids := make([]int, 0, n)
		for i := 0; i < n; i++ {
			id, err := s.CreateTask(worker, KindDispatch, "i", nil, nil, 0, 0, nil)
			if err != nil {
				t.Fatalf("create task %d: %v", i, err)
			}
			if err := s.SetTaskStatus(id, TaskRunning, ""); err != nil {
				t.Fatalf("start task %d: %v", i, err)
			}
			ids = append(ids, id)
		}

		var wg sync.WaitGroup
		var okCount, rejected, unexpected int32
		for _, id := range ids {
			wg.Add(2)
			go func(id int) {
				defer wg.Done()
				err := s.SetTaskStatus(id, TaskDone, "done")
				switch {
				case err == nil:
					atomic.AddInt32(&okCount, 1)
				case errors.Is(err, ErrInvalidTransition):
					atomic.AddInt32(&rejected, 1)
				default:
					atomic.AddInt32(&unexpected, 1)
				}
			}(id)
			go func(id int) {
				defer wg.Done()
				err := s.SetTaskStatus(id, TaskCanceled, "cancel")
				switch {
				case err == nil:
					atomic.AddInt32(&okCount, 1)
				case errors.Is(err, ErrInvalidTransition):
					atomic.AddInt32(&rejected, 1)
				default:
					atomic.AddInt32(&unexpected, 1)
				}
			}(id)
		}
		wg.Wait()

		if got := atomic.LoadInt32(&okCount); got != n {
			t.Errorf("winners = %d, want %d (one per task)", got, n)
		}
		if got := atomic.LoadInt32(&rejected); got != n {
			t.Errorf("rejected = %d, want %d", got, n)
		}
		if got := atomic.LoadInt32(&unexpected); got != 0 {
			t.Errorf("unexpected errors = %d, want 0", got)
		}
		for _, id := range ids {
			got, err := s.GetTask(id)
			if err != nil {
				t.Fatalf("get task %d: %v", id, err)
			}
			if got.Status != TaskDone && got.Status != TaskCanceled {
				t.Errorf("task %d final status = %q, want done|canceled", id, got.Status)
			}
		}
	})
}
