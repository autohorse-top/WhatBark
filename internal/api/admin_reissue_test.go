package api

import (
	"net/http"
	"strings"
	"testing"

	"whatbark/internal/store"
)

// S-token: POST /api/employees/reissue-tokens — batch reissue semantics.
// DB stores only sha256 digests, so "export" is a reissue: old tokens die,
// fresh plaintexts are returned exactly once.
func TestReissueEmployeeTokens(t *testing.T) {
	e := newEnv(t)
	dept, err := e.st.CreateDepartment("ReissueDept", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, tokA := makeEmployee(t, e, "ri-a", store.KindAI, dept)
	b, tokB := makeEmployee(t, e, "ri-b", store.KindAI, dept)

	// Baseline: both tokens authenticate.
	for name, tok := range map[string]string{"ri-a": tokA, "ri-b": tokB} {
		if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/me", tok, nil); code != http.StatusOK {
			t.Fatalf("%s baseline /api/me: got %d want 200", name, code)
		}
	}

	// Reissue both, with a duplicate id to exercise dedup.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/employees/reissue-tokens", adminTok,
		map[string]any{"ids": []int{a, b, a}})
	if code != http.StatusOK {
		t.Fatalf("reissue: status %d body=%v", code, body)
	}
	tokens, _ := body["tokens"].(map[string]any)
	if len(tokens) != 2 {
		t.Fatalf("tokens len = %d want 2 (dedup): %v", len(tokens), tokens)
	}
	newA, _ := tokens["ri-a"].(string)
	newB, _ := tokens["ri-b"].(string)
	if len(newA) != 32 || len(newB) != 32 {
		t.Fatalf("token lengths = %d/%d, want 32/32", len(newA), len(newB))
	}
	if newA == newB {
		t.Fatal("two reissued tokens are identical")
	}

	// Old tokens are dead.
	for name, tok := range map[string]string{"ri-a": tokA, "ri-b": tokB} {
		if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/me", tok, nil); code != http.StatusUnauthorized {
			t.Fatalf("%s old token: got %d want 401", name, code)
		}
	}
	// New tokens authenticate as the right employee.
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/me", newA, nil)
	if code != http.StatusOK {
		t.Fatalf("new token /api/me: got %d", code)
	}
	if body["admin"] != false || body["employee_id"].(float64) != float64(a) {
		t.Fatalf("new token identity = %v, want employee %d", body, a)
	}

	// Audit row recorded by the adminOnly wrapper.
	items, err := e.st.AuditList(100)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	found := false
	for _, it := range items {
		if it.Action == "employee.reissue_tokens" &&
			it.ActorKind == "admin" &&
			it.Target == "/api/employees/reissue-tokens" &&
			strings.Contains(it.Detail, "ids=") {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit log missing reissue row: %+v", items)
	}
}

func TestReissueEmployeeTokensValidation(t *testing.T) {
	e := newEnv(t)
	url := e.srv.URL + "/api/employees/reissue-tokens"

	// Empty ids.
	if code, _ := req(t, http.MethodPost, url, adminTok, map[string]any{"ids": []int{}}); code != http.StatusBadRequest {
		t.Fatalf("empty ids: got %d want 400", code)
	}
	// Over the 100-cap (checked before existence).
	big := make([]int, 101)
	for i := range big {
		big[i] = 9000 + i
	}
	if code, _ := req(t, http.MethodPost, url, adminTok, map[string]any{"ids": big}); code != http.StatusBadRequest {
		t.Fatalf("101 ids: got %d want 400", code)
	}
	// Unknown id.
	code, body := req(t, http.MethodPost, url, adminTok, map[string]any{"ids": []int{99999}})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown id: got %d want 400", code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "99999") {
		t.Fatalf("error %q should name the missing id", msg)
	}

	// Authz: employee token 403, anonymous 401.
	dept, _ := e.st.CreateDepartment("ReissueAuthz", nil)
	_, empTok := makeEmployee(t, e, "ri-authz", store.KindAI, dept)
	if code, _ := req(t, http.MethodPost, url, empTok, map[string]any{"ids": []int{1}}); code != http.StatusForbidden {
		t.Fatalf("employee token: got %d want 403", code)
	}
	if code, _ := req(t, http.MethodPost, url, "", map[string]any{"ids": []int{1}}); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: got %d want 401", code)
	}
}
