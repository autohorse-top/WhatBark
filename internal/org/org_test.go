package org

import (
	"reflect"
	"testing"
)

func intp(v int) *int { return &v }

// 4-level org:
//
//	dept1 (root, leader L1)
//	  dept2 (leader L2)
//	    dept3 (no leader)
//	      dept4 (leader L4)
//
// Employees: L1, L2, L4 in their own depts; W1 in dept3; W2 in dept4.
func fourLevel() Snapshot {
	return NewSnapshot(
		[]Department{
			{ID: 1, ParentID: nil, LeaderID: intp(100)},
			{ID: 2, ParentID: intp(1), LeaderID: intp(200)},
			{ID: 3, ParentID: intp(2), LeaderID: nil},
			{ID: 4, ParentID: intp(3), LeaderID: intp(400)},
		},
		[]EmployeeRef{
			{ID: 100, DepartmentID: 1},
			{ID: 200, DepartmentID: 2},
			{ID: 400, DepartmentID: 4},
			{ID: 1, DepartmentID: 3}, // W1
			{ID: 2, DepartmentID: 4}, // W2
		},
	)
}

func TestReportingChain(t *testing.T) {
	cases := []struct {
		name    string
		empID   int
		want    []ChainNode
		wantErr bool
	}{
		{"worker in dept with no direct leader", 1,
			[]ChainNode{{0, true}, {200, false}, {100, false}}, false},
		{"worker under dept leader", 2,
			[]ChainNode{{400, false}, {0, true}, {200, false}, {100, false}}, false},
		{"dept leader skips self", 400,
			[]ChainNode{{0, true}, {200, false}, {100, false}}, false},
		{"root leader", 100, []ChainNode{}, false},
		{"unknown employee", 999, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReportingChain(fourLevel(), c.empID)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (chain=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("chain = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDispatchChainIsReverse(t *testing.T) {
	chain, err := ReportingChain(fourLevel(), 1)
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := DispatchChain(fourLevel(), 1)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]ChainNode, len(chain))
	for i, n := range chain {
		want[len(chain)-1-i] = n
	}
	if !reflect.DeepEqual(reverse, want) {
		t.Fatalf("dispatch = %+v, want %+v", reverse, want)
	}
}

func TestCycleFailsClosed(t *testing.T) {
	s := NewSnapshot(
		[]Department{
			{ID: 1, ParentID: intp(2), LeaderID: intp(100)},
			{ID: 2, ParentID: intp(1), LeaderID: intp(200)},
		},
		[]EmployeeRef{{ID: 100, DepartmentID: 1}, {ID: 200, DepartmentID: 2}},
	)
	if _, err := ReportingChain(s, 100); err == nil {
		t.Fatal("cycle accepted, want error")
	}
}

func TestDanglingParentStops(t *testing.T) {
	s := NewSnapshot(
		[]Department{
			{ID: 1, ParentID: intp(99), LeaderID: intp(100)}, // parent 99 does not exist
		},
		[]EmployeeRef{{ID: 1, DepartmentID: 1}},
	)
	got, err := ReportingChain(s, 1)
	if err != nil {
		t.Fatalf("dangling parent must not error: %v", err)
	}
	if len(got) != 1 || got[0].EmpID != 100 {
		t.Fatalf("chain = %+v, want [100]", got)
	}
}

func TestIsLeaderAndFirstLeader(t *testing.T) {
	s := fourLevel()
	if !IsLeader(s, 200) || IsLeader(s, 1) {
		t.Fatal("IsLeader wrong")
	}
	if id, ok := FirstLeader(s, 1); !ok || id != 200 {
		t.Fatalf("FirstLeader(1) = %d,%v, want 200,true", id, ok)
	}
	if id, ok := FirstLeader(s, 100); ok || id != 0 {
		t.Fatalf("FirstLeader(root leader) = %d,%v, want 0,false", id, ok)
	}
	if id, ok := LeadersOf(s, 2); !ok || id != 200 {
		t.Fatalf("LeadersOf(2) = %d,%v, want 200,true", id, ok)
	}
	if _, ok := LeadersOf(s, 3); ok {
		t.Fatal("LeadersOf(dept without leader) should be false")
	}
}
