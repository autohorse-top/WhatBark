package store

import (
	"fmt"
	"testing"
)

// H-7: audit_log append/list semantics.

func TestAuditAppendList(t *testing.T) {
	s := openTestStore(t)

	if err := s.AuditAppend("admin", 0, "employee.create", "/api/employees", ""); err != nil {
		t.Fatalf("AuditAppend 1: %v", err)
	}
	if err := s.AuditAppend("employee", 3, "task.cancel", "/api/tasks/7", "done"); err != nil {
		t.Fatalf("AuditAppend 2: %v", err)
	}
	if err := s.AuditAppend("anon", 0, "auth.fail", "/api/me", "missing or invalid token"); err != nil {
		t.Fatalf("AuditAppend 3: %v", err)
	}

	got, err := s.AuditList(0) // default limit
	if err != nil {
		t.Fatalf("AuditList: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("AuditList: %d rows, want 3", len(got))
	}
	// Newest first: auth.fail (last inserted) must come first.
	if got[0].Action != "auth.fail" || got[1].Action != "task.cancel" || got[2].Action != "employee.create" {
		t.Fatalf("order: got %q,%q,%q want newest-first", got[0].Action, got[1].Action, got[2].Action)
	}
	if got[0].ActorKind != "anon" || got[0].ActorID != 0 {
		t.Fatalf("row0 actor: got %s/%d want anon/0", got[0].ActorKind, got[0].ActorID)
	}
	if got[1].Detail != "done" || got[1].Target != "/api/tasks/7" {
		t.Fatalf("row1 target/detail: got %q/%q", got[1].Target, got[1].Detail)
	}
	if got[0].TS == "" {
		t.Fatal("ts not populated")
	}
}

func TestAuditListLimit(t *testing.T) {
	s := openTestStore(t)

	const n = 12
	for i := 0; i < n; i++ {
		if err := s.AuditAppend("admin", 0, "x", fmt.Sprintf("/api/x/%d", i), ""); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// limit=3: the three NEWEST rows only (ids 9,10,11 -> /api/x/11 first).
	got, err := s.AuditList(3)
	if err != nil {
		t.Fatalf("AuditList(3): %v", err)
	}
	if len(got) != 3 || got[0].Target != "/api/x/11" || got[2].Target != "/api/x/9" {
		t.Fatalf("AuditList(3): got %d rows, first=%q last=%q", len(got), got[0].Target, got[2].Target)
	}

	// Cap: more than 500 rows exist, limit > 500 must clamp to 500.
	s2 := openTestStore(t)
	for i := 0; i < 520; i++ {
		if err := s2.AuditAppend("admin", 0, "cap", fmt.Sprintf("t%d", i), ""); err != nil {
			t.Fatalf("cap append %d: %v", i, err)
		}
	}
	capped, err := s2.AuditList(9999)
	if err != nil {
		t.Fatalf("AuditList(9999): %v", err)
	}
	if len(capped) != 500 {
		t.Fatalf("AuditList(9999): %d rows, want 500 (clamped)", len(capped))
	}
}
