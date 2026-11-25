// Package org computes reporting/dispatch chains over the org tree.
// All functions are pure: they operate on a Snapshot and never touch storage.
package org

import "fmt"

// PredefinedRoles is the v1 role catalog shown in the UI.
// Roles are labels only; routing depends on the org tree, not on roles.
var PredefinedRoles = []string{"leader", "worker", "reviewer", "qa", "ops"}

// Department is the routing-relevant part of a department row.
type Department struct {
	ID       int
	ParentID *int
	LeaderID *int
}

// EmployeeRef is the routing-relevant part of an employee row.
type EmployeeRef struct {
	ID           int
	DepartmentID int
}

// Snapshot is an immutable view of the org tree.
type Snapshot struct {
	Departments map[int]Department
	Employees   map[int]EmployeeRef
}

// NewSnapshot builds a Snapshot from row-level data.
func NewSnapshot(departments []Department, employees []EmployeeRef) Snapshot {
	ds := make(map[int]Department, len(departments))
	for _, d := range departments {
		ds[d.ID] = d
	}
	es := make(map[int]EmployeeRef, len(employees))
	for _, e := range employees {
		es[e.ID] = e
	}
	return Snapshot{Departments: ds, Employees: es}
}

// ChainNode is one hop in a reporting chain.
type ChainNode struct {
	EmpID int
	IsGap bool // this segment has no leader; chain continues upward
}

// ReportingChain returns the chain from empID up to the root, nearest first.
//   - The employee itself is excluded.
//
// - If empID leads its own department, that segment is skipped (no self-loop).
// - A department without a leader emits a gap node (EmpID 0) and the chain continues.
// - A dangling parent stops the chain.
// - A department cycle returns an error (fail closed).
func ReportingChain(s Snapshot, empID int) ([]ChainNode, error) {
	emp, ok := s.Employees[empID]
	if !ok {
		return nil, fmt.Errorf("org: unknown employee %d", empID)
	}
	var out []ChainNode
	seen := map[int]bool{}
	deptID := emp.DepartmentID
	for deptID != 0 {
		d, ok := s.Departments[deptID]
		if !ok {
			break // dangling parent: stop
		}
		if seen[deptID] {
			return nil, fmt.Errorf("org: department cycle at %d", deptID)
		}
		seen[deptID] = true
		if d.LeaderID != nil {
			if *d.LeaderID != empID {
				out = append(out, ChainNode{EmpID: *d.LeaderID})
			}
		} else {
			out = append(out, ChainNode{EmpID: 0, IsGap: true})
		}
		if d.ParentID == nil {
			break
		}
		deptID = *d.ParentID
	}
	if out == nil {
		out = []ChainNode{}
	}
	return out, nil
}

// DispatchChain is the reverse of the reporting chain (root side first).
// Gap nodes are carried over verbatim.
func DispatchChain(s Snapshot, empID int) ([]ChainNode, error) {
	chain, err := ReportingChain(s, empID)
	if err != nil {
		return nil, err
	}
	out := make([]ChainNode, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		out = append(out, chain[i])
	}
	return out, nil
}

// IsLeader reports whether empID is a department leader.
func IsLeader(s Snapshot, empID int) bool {
	for _, d := range s.Departments {
		if d.LeaderID != nil && *d.LeaderID == empID {
			return true
		}
	}
	return false
}

// FirstLeader returns the nearest non-gap leader above empID, if any.
func FirstLeader(s Snapshot, empID int) (int, bool) {
	chain, err := ReportingChain(s, empID)
	if err != nil {
		return 0, false
	}
	for _, n := range chain {
		if !n.IsGap {
			return n.EmpID, true
		}
	}
	return 0, false
}

// LeadersOf returns the leader of the department an employee belongs to, if set.
func LeadersOf(s Snapshot, deptID int) (int, bool) {
	d, ok := s.Departments[deptID]
	if !ok || d.LeaderID == nil {
		return 0, false
	}
	return *d.LeaderID, true
}
