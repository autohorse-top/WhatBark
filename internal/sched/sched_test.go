package sched

import (
	"testing"
	"time"

	"whatbark/internal/flow"
	"whatbark/internal/store"
)

func testStore(t *testing.T) (*store.Store, int) {
	t.Helper()
	db := t.TempDir() + "/sched.db"
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dept, err := st.CreateDepartment("D", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := st.CreateEmployee("W", "worker", store.KindAI, dept)
	if err != nil {
		t.Fatal(err)
	}
	return st, w
}

func countTasks(st *store.Store, empID int, kind string) int {
	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE employee_id = ? AND kind = ?`, empID, kind,
	).Scan(&n); err != nil {
		t2 := -1
		_ = t2
	}
	return n
}

func TestScheduleFires(t *testing.T) {
	st, w := testStore(t)
	fl := flow.New(st, nil, nil)
	sc := New(st, fl)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sc.Now = func() time.Time { return now }

	sid, err := st.CreateSchedule("daily", w, 3600, "do the daily thing")
	if err != nil {
		t.Fatal(err)
	}

	// First tick: no last_fired → fires immediately.
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(st, w, store.KindScheduled); n != 1 {
		t.Fatalf("after first step: %d scheduled tasks, want 1", n)
	}

	// One second later: not due (every=3600s).
	now = now.Add(1 * time.Second)
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(st, w, store.KindScheduled); n != 1 {
		t.Fatalf("after +1s: %d scheduled tasks, want 1 (not due)", n)
	}

	// Hour later: due again.
	now = now.Add(3600 * time.Second)
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(st, w, store.KindScheduled); n != 2 {
		t.Fatalf("after +3600s: %d scheduled tasks, want 2", n)
	}

	// Disable → no more fires.
	if err := st.SetScheduleEnabled(sid, false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3600 * time.Second)
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(st, w, store.KindScheduled); n != 2 {
		t.Fatalf("after disable: %d scheduled tasks, want 2", n)
	}
}

func TestSelfReviewInjection(t *testing.T) {
	st, w := testStore(t)
	fl := flow.New(st, nil, nil)
	sc := New(st, fl)
	// Anchor on the real clock: the injected task's created_at is stamped by
	// SQLite's datetime('now'), so a hardcoded fake "now" in the past makes
	// now.Sub(last) negative and the interval check always skips.
	now := time.Now()
	sc.Now = func() time.Time { return now }

	if err := st.SetSetting("selfreview_interval_seconds", "10"); err != nil {
		t.Fatal(err)
	}

	// First tick: no selfreview task yet → inject.
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(st, w, store.KindSelfReview); n != 1 {
		t.Fatalf("after first step: %d selfreview tasks, want 1", n)
	}

	// One second later: still within interval AND task pending → no new.
	now = now.Add(1 * time.Second)
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(st, w, store.KindSelfReview); n != 1 {
		t.Fatalf("after +1s: %d selfreview tasks, want 1", n)
	}

	// Complete the selfreview; advance past interval → inject again.
	tasks, _ := st.ListTasksByEmployee(w, nil)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if err := st.SetTaskStatus(tasks[0].ID, store.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := fl.FinishTask(tasks[0].ID, "done reviewing", true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(st, w, store.KindSelfReview); n != 2 {
		t.Fatalf("after interval: %d selfreview tasks, want 2", n)
	}
}

// TestSelfReviewNoFloodInInterval guards the created_at parse path: once a
// selfreview task exists and is completed, steps inside the interval must not
// inject further selfreviews (regression: created_at from the schema default
// datetime('now') is "2006-01-02 15:04:05", not RFC3339; a parse failure used
// to make hasLast=false and flood one selfreview per AI employee per tick).
func TestSelfReviewNoFloodInInterval(t *testing.T) {
	st, w := testStore(t)
	fl := flow.New(st, nil, nil)
	sc := New(st, fl)
	now := time.Now()
	sc.Now = func() time.Time { return now }

	if err := st.SetSetting("selfreview_interval_seconds", "10"); err != nil {
		t.Fatal(err)
	}

	// First tick: no selfreview yet -> inject.
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(st, w, store.KindSelfReview); n != 1 {
		t.Fatalf("after first step: %d selfreview tasks, want 1", n)
	}

	// Complete it, then step again inside the interval: the real created_at
	// must be respected, so no second injection even though nothing is pending.
	tasks, err := st.ListTasksByEmployee(w, nil)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(tasks), err)
	}
	if err := st.SetTaskStatus(tasks[0].ID, store.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := fl.FinishTask(tasks[0].ID, "ok", true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(1 * time.Second)
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	if n := countTasks(st, w, store.KindSelfReview); n != 1 {
		t.Fatalf("inside interval after completion: %d selfreview tasks, want 1 (no flood)", n)
	}
}

func TestSelfReviewSkipsHumans(t *testing.T) {
	st, _ := testStore(t)
	dept, _ := st.CreateDepartment("DH", nil)
	if _, err := st.CreateEmployee("H", "human", store.KindHuman, dept); err != nil {
		t.Fatal(err)
	}
	fl := flow.New(st, nil, nil)
	sc := New(st, fl)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sc.Now = func() time.Time { return now }
	if err := st.SetSetting("selfreview_interval_seconds", "1"); err != nil {
		t.Fatal(err)
	}
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}
	emps, _ := st.ListEmployees()
	for _, e := range emps {
		if e.Kind == store.KindHuman {
			if n := countTasks(st, e.ID, store.KindSelfReview); n != 0 {
				t.Fatalf("human employee %d got %d selfreview tasks, want 0", e.ID, n)
			}
		}
	}
}
