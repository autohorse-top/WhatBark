package api

import (
	"fmt"
	"net/http"
	"testing"

	"whatbark/internal/store"
)

// TestSetGroupPolicyAPI (S30): POST /api/groups/{id}/policy — permission
// matrix (boss, creator, member, non-member, unknown group) and validation
// (malformed id, empty body, range checks, boundaries, partial updates).
func TestSetGroupPolicyAPI(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)
	gid := makeGroup(t, e, l1tok, "policy-g", []int{w1})
	policyURL := fmt.Sprintf("%s/api/groups/%d/policy", e.srv.URL, gid)
	full := map[string]any{"default_priority": 42, "sla_timeout_seconds": 300}

	// Creator (employee) may set the policy: 200 with both fields echoed.
	code, got := req(t, http.MethodPost, policyURL, l1tok, full)
	if code != http.StatusOK {
		t.Fatalf("creator set policy: got %d, want 200 (body %v)", code, got)
	}
	if got["default_priority"] != float64(42) || got["sla_timeout_seconds"] != float64(300) {
		t.Errorf("set policy response = %v, want 42/300", got)
	}
	g, err := e.st.GetGroup(gid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if g.DefaultPriority != 42 || g.SlaTimeoutSeconds != 300 {
		t.Errorf("stored policy = %d/%d, want 42/300", g.DefaultPriority, g.SlaTimeoutSeconds)
	}

	// Partial update by the boss: sla changes, dp must persist.
	code, got = req(t, http.MethodPost, policyURL, adminTok,
		map[string]any{"sla_timeout_seconds": 600})
	if code != http.StatusOK {
		t.Fatalf("boss partial update: got %d, want 200 (body %v)", code, got)
	}
	if got["default_priority"] != float64(42) {
		t.Errorf("partial update: default_priority = %v, want 42 (unchanged)", got["default_priority"])
	}
	g, _ = e.st.GetGroup(gid)
	if g.SlaTimeoutSeconds != 600 {
		t.Errorf("stored sla = %d, want 600", g.SlaTimeoutSeconds)
	}

	// GET reflects the policy columns.
	code, got = req(t, http.MethodGet, fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("GET group: got %d", code)
	}
	if got["default_priority"] != float64(42) || got["sla_timeout_seconds"] != float64(600) {
		t.Errorf("GET group policy = %v/%v, want 42/600", got["default_priority"], got["sla_timeout_seconds"])
	}

	// Permission matrix.
	if code, _ := req(t, http.MethodPost, policyURL, w1tok, full); code != http.StatusForbidden {
		t.Errorf("member-but-not-creator: got %d, want 403", code)
	}
	depts, err := e.st.ListDepartments()
	if err != nil {
		t.Fatalf("ListDepartments: %v", err)
	}
	_, outTok := makeEmployee(t, e, "E-out", store.KindAI, depts[0].ID)
	if code, _ := req(t, http.MethodPost, policyURL, outTok, full); code != http.StatusForbidden {
		t.Errorf("non-member: got %d, want 403", code)
	}

	// Validation failures.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/groups/abc/policy", l1tok, full); code != http.StatusBadRequest {
		t.Errorf("malformed id: got %d, want 400", code)
	}
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/groups/99999/policy", adminTok, full); code != http.StatusNotFound {
		t.Errorf("unknown group: got %d, want 404", code)
	}
	bad := []map[string]any{
		{},                        // nothing to set
		{"default_priority": -1},  // below range
		{"default_priority": 100}, // above range
		{"sla_timeout_seconds": -1},
		{"sla_timeout_seconds": 2592001},
	}
	for i, b := range bad {
		if code, got := req(t, http.MethodPost, policyURL, adminTok, b); code != http.StatusBadRequest {
			t.Errorf("case %d (%v): got %d, want 400 (body %v)", i, b, code, got)
		}
	}
	// Range boundaries are accepted.
	for _, b := range []map[string]any{
		{"default_priority": 0},
		{"default_priority": 99},
		{"sla_timeout_seconds": 0},
		{"sla_timeout_seconds": 2592000},
	} {
		if code, got := req(t, http.MethodPost, policyURL, adminTok, b); code != http.StatusOK {
			t.Errorf("boundary %v: got %d, want 200 (body %v)", b, code, got)
		}
	}
}

// TestGroupDispatchDefaultPriorityAPI (S30): a group dispatch without an
// explicit priority inherits the group's default_priority; an explicit
// priority in the body wins.
func TestGroupDispatchDefaultPriorityAPI(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, _ := topo(t, e)
	gid := makeGroup(t, e, l1tok, "prio-dispatch", []int{w1})
	code, _ := req(t, http.MethodPost, fmt.Sprintf("%s/api/groups/%d/policy", e.srv.URL, gid), l1tok,
		map[string]any{"default_priority": 77})
	if code != http.StatusOK {
		t.Fatalf("set default priority: got %d", code)
	}

	// No priority in the body -> the group default applies.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gid, "kind": "dispatch", "to": w1, "body": "inherited"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch without priority: got %d (body %v)", code, body)
	}
	t1, err := e.st.GetTask(int(body["task_id"].(float64)))
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if t1.Priority != 77 {
		t.Errorf("inherited priority = %d, want 77", t1.Priority)
	}

	// Explicit priority in the body wins over the group default.
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"group": gid, "kind": "dispatch", "to": w1, "body": "explicit", "priority": 5})
	if code != http.StatusCreated {
		t.Fatalf("dispatch with priority: got %d (body %v)", code, body)
	}
	t2, err := e.st.GetTask(int(body["task_id"].(float64)))
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if t2.Priority != 5 {
		t.Errorf("explicit priority = %d, want 5", t2.Priority)
	}
}
