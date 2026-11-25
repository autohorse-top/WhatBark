package api

import (
	"net/http"
	"testing"

	"whatbark/internal/store"
)

// H-7: audit log API behavior.

// TestAuditAPIMixed covers the acceptance criteria: bad token produces an
// auth.fail row, non-admin gets 403, and an admin-created employee shows up
// in the admin-only audit list.
func TestAuditAPIMixed(t *testing.T) {
	e := newEnv(t)

	// 1. Bad token -> 401 and an auth.fail audit row.
	code, _ := req(t, http.MethodGet, e.srv.URL+"/api/audit", "bogus-token", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d want 401", code)
	}

	// 2. Non-admin employee token -> 403 on /api/audit.
	a, err := e.st.CreateDepartment("AuditDept", nil)
	if err != nil {
		t.Fatalf("create department: %v", err)
	}
	_, empTok := makeEmployee(t, e, "AudE1", store.KindAI, a)
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/audit", empTok, nil); code != http.StatusForbidden {
		t.Fatalf("non-admin audit: got %d want 403", code)
	}

	// 3. Admin creates an employee via the API -> audited as employee.create.
	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/employees", adminTok,
		map[string]any{"name": "AudE2", "role": "worker", "kind": store.KindAI, "department_id": a})
	if code != http.StatusCreated {
		t.Fatalf("create employee: got %d want 201", code)
	}

	// 4. Admin list: newest first, must contain both employee.create and auth.fail.
	code, body := req(t, http.MethodGet, e.srv.URL+"/api/audit", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("audit list: got %d want 200", code)
	}
	raw, _ := body["items"].([]any)
	if len(raw) < 2 {
		t.Fatalf("items: got %d, want >= 2", len(raw))
	}
	var sawCreate, sawAuthFail bool
	for _, it := range raw {
		m, _ := it.(map[string]any)
		act, _ := m["action"].(string)
		switch act {
		case "employee.create":
			sawCreate = true
			if m["actor_kind"] != "admin" {
				t.Errorf("employee.create actor_kind = %v, want admin", m["actor_kind"])
			}
			if m["target"] != "/api/employees" {
				t.Errorf("employee.create target = %v", m["target"])
			}
		case "auth.fail":
			sawAuthFail = true
			if m["actor_kind"] != "anon" {
				t.Errorf("auth.fail actor_kind = %v, want anon", m["actor_kind"])
			}
		}
	}
	if !sawCreate {
		t.Error("missing employee.create audit entry")
	}
	if !sawAuthFail {
		t.Error("missing auth.fail audit entry")
	}
}

// TestAuditLimitValidation pins the limit param contract.
func TestAuditLimitValidation(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=abc", "?limit=9999"} {
		if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/audit"+q, adminTok, nil); code != http.StatusBadRequest {
			t.Errorf("limit %s: got %d, want 400", q, code)
		}
	}
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/audit?limit=5", adminTok, nil); code != http.StatusOK {
		t.Errorf("limit=5: got %d, want 200", code)
	}
}
