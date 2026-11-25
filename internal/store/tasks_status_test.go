package store

import "testing"

// TestTaskStatuses (S28): the batched id -> live-status lookup used by group
// thread message rows. Table-driven: empty/nil input is a no-op, unknown ids
// are absent from the map, statuses reflect state transitions, duplicates
// collapse.
func TestTaskStatuses(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	newTask := func() int {
		t.Helper()
		id, err := s.CreateTask(worker, KindDispatch, "x", nil, nil, leader, 0, nil)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		return id
	}
	q1 := newTask()
	q2 := newTask()
	r1 := newTask()
	d1 := newTask()
	f1 := newTask()
	// State machine: queued -> running -> done|failed (tasks.go allowedMoves).
	for _, id := range []int{r1, d1, f1} {
		if err := s.SetTaskStatus(id, TaskRunning, ""); err != nil {
			t.Fatalf("SetTaskStatus(%d, running): %v", id, err)
		}
	}
	if err := s.SetTaskStatus(d1, TaskDone, "done"); err != nil {
		t.Fatalf("SetTaskStatus(%d, done): %v", d1, err)
	}
	if err := s.SetTaskStatus(f1, TaskFailed, "boom"); err != nil {
		t.Fatalf("SetTaskStatus(%d, failed): %v", f1, err)
	}

	tests := []struct {
		name string
		ids  []int
		want map[int]string
	}{
		{"empty slice returns empty map", []int{}, map[int]string{}},
		{"nil slice returns empty map", nil, map[int]string{}},
		{"single id", []int{q1}, map[int]string{q1: TaskQueued}},
		{"mixed statuses", []int{q1, r1, d1, f1},
			map[int]string{q1: TaskQueued, r1: TaskRunning, d1: TaskDone, f1: TaskFailed}},
		{"unknown ids are absent", []int{q1, 999999}, map[int]string{q1: TaskQueued}},
		{"duplicates collapse", []int{q1, q1, q2}, map[int]string{q1: TaskQueued, q2: TaskQueued}},
	}
	// F-2: id sets beyond SQLite's per-statement variable limit (32766) must
	// work via batching — a single unbounded IN list used to 500 the request.
	big := make([]int, 0, 35000)
	big = append(big, q1, r1, d1)
	for i := 100001; i < 100001+35000-3; i++ {
		big = append(big, i)
	}
	tests = append(tests, struct {
		name string
		ids  []int
		want map[int]string
	}{
		name: "35000 ids (exceeds 32766 var limit) batch correctly",
		ids:  big,
		want: map[int]string{q1: TaskQueued, r1: TaskRunning, d1: TaskDone},
	})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.TaskStatuses(tc.ids)
			if err != nil {
				t.Fatalf("TaskStatuses(%v): %v", tc.ids, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("TaskStatuses(%v) = %v (len %d), want %v (len %d)",
					tc.ids, got, len(got), tc.want, len(tc.want))
			}
			for id, want := range tc.want {
				st, ok := got[id]
				if !ok || st != want {
					t.Errorf("status[%d] = %q (present=%v), want %q", id, st, ok, want)
				}
			}
		})
	}
}
