package store

import "testing"

// v1-hardening H-2: batch lookups that replace the per-row N+1 pattern in
// handleGroupTasks. Contract: unknown ids absent from the result map; empty
// input returns an empty map without SQL; chunking keeps placeholder counts
// under SQLite's variable limit (F-2 pattern).

func TestEmployeeNamesBatch(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	got, err := s.EmployeeNames(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("EmployeeNames(nil) = %v, %v; want empty map, no error", got, err)
	}

	got, err = s.EmployeeNames([]int{leader, worker, 999999})
	if err != nil {
		t.Fatalf("EmployeeNames: %v", err)
	}
	if len(got) != 2 || got[leader] != "alice" || got[worker] != "bob" {
		t.Fatalf("EmployeeNames = %v, want alice/bob with unknown id absent", got)
	}

	got, err = s.EmployeeNames([]int{worker, worker})
	if err != nil {
		t.Fatalf("EmployeeNames(dups): %v", err)
	}
	if len(got) != 1 || got[worker] != "bob" {
		t.Fatalf("EmployeeNames(dups) = %v, want dedup to one entry", got)
	}
}

func TestProjectNamesBatch(t *testing.T) {
	s := openTestStore(t)

	got, err := s.ProjectNames(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("ProjectNames(nil) = %v, %v; want empty map, no error", got, err)
	}

	p1, err := s.CreateProject("proj-a", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	p2, err := s.CreateProject("proj-b", "", nil, nil)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got, err = s.ProjectNames([]int{p1, p2, 999999})
	if err != nil {
		t.Fatalf("ProjectNames: %v", err)
	}
	if len(got) != 2 || got[p1] != "proj-a" || got[p2] != "proj-b" {
		t.Fatalf("ProjectNames = %v, want proj-a/proj-b with unknown id absent", got)
	}

	// A deleted project disappears from the map (same tolerance pattern the
	// API layer relies on for project_name omission).
	if err := s.DeleteProject(p1); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	got, err = s.ProjectNames([]int{p1, p2})
	if err != nil {
		t.Fatalf("ProjectNames(after delete): %v", err)
	}
	if len(got) != 1 || got[p2] != "proj-b" {
		t.Fatalf("ProjectNames(after delete) = %v, want only proj-b", got)
	}
}

func TestDispatchMessageIDsForGroupTasks(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)

	gid, err := s.CreateGroup("g", leader, []int{worker}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	t1, err := s.CreateTask(worker, KindDispatch, "t1", nil, nil, leader, 0, intp(gid))
	if err != nil {
		t.Fatalf("CreateTask t1: %v", err)
	}
	t2, err := s.CreateTask(worker, KindDispatch, "t2", nil, nil, leader, 0, intp(gid))
	if err != nil {
		t.Fatalf("CreateTask t2: %v", err)
	}

	m1, err := s.CreateGroupMessage(gid, MsgDispatch, "do t1", intp(leader), nil, nil)
	if err != nil {
		t.Fatalf("CreateGroupMessage t1: %v", err)
	}
	if err := s.SetMessageTaskID(m1, t1); err != nil {
		t.Fatalf("SetMessageTaskID t1: %v", err)
	}
	// A second dispatch message for t1: the singular contract returns the
	// oldest (ORDER BY id LIMIT 1), so the batch version must too.
	m2, err := s.CreateGroupMessage(gid, MsgDispatch, "do t1 again", intp(leader), nil, nil)
	if err != nil {
		t.Fatalf("CreateGroupMessage t1b: %v", err)
	}
	if err := s.SetMessageTaskID(m2, t1); err != nil {
		t.Fatalf("SetMessageTaskID t1b: %v", err)
	}

	if got, err := s.DispatchMessageIDsForGroupTasks(gid, nil); err != nil || len(got) != 0 {
		t.Fatalf("DispatchMessageIDsForGroupTasks(nil) = %v, %v; want empty", got, err)
	}

	got, err := s.DispatchMessageIDsForGroupTasks(gid, []int{t1, t2, 999999})
	if err != nil {
		t.Fatalf("DispatchMessageIDsForGroupTasks: %v", err)
	}
	if len(got) != 1 || got[t1] != m1 {
		t.Fatalf("DispatchMessageIDsForGroupTasks = %v, want {t1: m1(oldest)}, t2/unknown absent", got)
	}
	if m1 > m2 {
		t.Fatalf("setup: m1(%d) should precede m2(%d)", m1, m2)
	}
}
