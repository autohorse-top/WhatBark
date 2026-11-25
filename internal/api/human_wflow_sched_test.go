package api

import (
	"fmt"
	"net/http"
	"testing"

	"whatbark/internal/store"
)

// humanTopo builds:
//
//	A (root, leader H1=human) <- B (leader L2=AI, workers W1,W2=AI)
//	C (root, no leader)        -> E9 (AI), H9 (human, no subordinates)
//
// H1 subordinates = {L2, W1, W2}; E9 and H9 are NOT H1 subordinates.
func humanTopo(t *testing.T, e *env) (h1 int, h1tok string, l2, w1, w2, e9, h9 int, h9tok string) {
	t.Helper()
	a, err := e.st.CreateDepartment("A", nil)
	if err != nil {
		t.Fatal(err)
	}
	h1, h1tok = makeEmployee(t, e, "H1", store.KindHuman, a)
	if err := e.st.SetDepartmentLeader(a, h1); err != nil {
		t.Fatal(err)
	}
	b, err := e.st.CreateDepartment("B", &a)
	if err != nil {
		t.Fatal(err)
	}
	l2, _ = makeEmployee(t, e, "L2", store.KindAI, b)
	if err := e.st.SetDepartmentLeader(b, l2); err != nil {
		t.Fatal(err)
	}
	w1, _ = makeEmployee(t, e, "W1", store.KindAI, b)
	w2, _ = makeEmployee(t, e, "W2", store.KindAI, b)
	c, err := e.st.CreateDepartment("C", nil)
	if err != nil {
		t.Fatal(err)
	}
	e9, _ = makeEmployee(t, e, "E9", store.KindAI, c)
	h9, h9tok = makeEmployee(t, e, "H9", store.KindHuman, c)
	return
}

func subSet(body map[string]any, key string) map[int]bool {
	raw, _ := body[key].([]any)
	out := make(map[int]bool, len(raw))
	for _, v := range raw {
		out[int(v.(float64))] = true
	}
	return out
}

func TestMeSubordinates(t *testing.T) {
	e := newEnv(t)
	_, h1tok, l2, w1, _, e9, _, h9tok := humanTopo(t, e)

	code, body := req(t, http.MethodGet, e.srv.URL+"/api/me", h1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("me H1: got %d want 200", code)
	}
	sub := subSet(body, "subordinates")
	for _, want := range []int{l2, w1} {
		if !sub[want] {
			t.Errorf("H1 subordinates = %v, want contains %d", sub, want)
		}
	}
	if sub[e9] {
		t.Errorf("H1 subordinates must not contain E9: %v", sub)
	}

	// H9 has no subordinates -> empty list.
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/me", h9tok, nil)
	if code != http.StatusOK {
		t.Fatalf("me H9: got %d want 200", code)
	}
	if got := subSet(body, "subordinates"); len(got) != 0 {
		t.Errorf("H9 subordinates = %v, want empty", got)
	}

	// Admin gets an empty array (not omitted).
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/me", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("me admin: got %d want 200", code)
	}
	if raw, _ := body["subordinates"].([]any); raw == nil {
		t.Errorf("admin /api/me missing subordinates array")
	}
}

func TestHumanWorkflowPermissions(t *testing.T) {
	e := newEnv(t)
	_, h1tok, _, w1, w2, e9, _, _ := humanTopo(t, e)

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/workflows", h1tok, map[string]any{"name": "h1wf"})
	if code != http.StatusCreated {
		t.Fatalf("H1 create workflow: got %d want 201", code)
	}
	wfID := int(body["id"].(float64))
	url := fmt.Sprintf("%s/api/workflows/%d", e.srv.URL, wfID)

	n1, body1 := req(t, http.MethodPost, url+"/nodes", h1tok, map[string]any{"employee_id": w1, "x": 1, "y": 2})
	if n1 != http.StatusCreated {
		t.Fatalf("H1 add W1 node: got %d want 201 (%v)", n1, body1)
	}
	nodeW1 := int(body1["id"].(float64))
	n2, body2 := req(t, http.MethodPost, url+"/nodes", h1tok, map[string]any{"employee_id": w2, "x": 3, "y": 4})
	if n2 != http.StatusCreated {
		t.Fatalf("H1 add W2 node: got %d want 201 (%v)", n2, body2)
	}
	nodeW2 := int(body2["id"].(float64))

	code, _ = req(t, http.MethodPost, url+"/nodes", h1tok, map[string]any{"employee_id": e9, "x": 5, "y": 6})
	if code != http.StatusForbidden {
		t.Errorf("H1 add E9 node: got %d want 403", code)
	}

	code, _ = req(t, http.MethodPost, url+"/edges", h1tok, map[string]any{"from_node_id": nodeW1, "to_node_id": nodeW2})
	if code != http.StatusCreated {
		t.Errorf("H1 add edge: got %d want 201", code)
	}

	code, _ = req(t, http.MethodPut, fmt.Sprintf("%s/nodes/%d", url, nodeW1), h1tok, map[string]any{"x": 9, "y": 9})
	if code != http.StatusOK {
		t.Errorf("H1 move node: got %d want 200", code)
	}
	code, _ = req(t, http.MethodDelete, fmt.Sprintf("%s/nodes/%d", url, nodeW2), h1tok, nil)
	if code != http.StatusOK {
		t.Errorf("H1 remove node: got %d want 200", code)
	}

	code, _ = req(t, http.MethodDelete, url, h1tok, nil)
	if code != http.StatusForbidden {
		t.Errorf("H1 delete workflow: got %d want 403", code)
	}

	code, _ = req(t, http.MethodPost, url+"/nodes", adminTok, map[string]any{"employee_id": e9, "x": 1, "y": 1})
	if code != http.StatusCreated {
		t.Errorf("admin add E9 node: got %d want 201", code)
	}
	code, _ = req(t, http.MethodDelete, url, adminTok, nil)
	if code != http.StatusOK {
		t.Errorf("admin delete workflow: got %d want 200", code)
	}
}

func TestAIEmployeeBlockedFromWorkflow(t *testing.T) {
	e := newEnv(t)
	_, _, _, _, _, _, _, _ = humanTopo(t, e)
	w1Tok := "tok-W1"

	code, _ := req(t, http.MethodPost, e.srv.URL+"/api/workflows", w1Tok, map[string]any{"name": "aiwf"})
	if code != http.StatusForbidden {
		t.Errorf("AI create workflow: got %d want 403", code)
	}
}

func TestHumanNoSubordinates(t *testing.T) {
	e := newEnv(t)
	_, _, _, w1, _, _, _, h9tok := humanTopo(t, e)

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/workflows", h9tok, map[string]any{"name": "h9wf"})
	if code != http.StatusCreated {
		t.Fatalf("H9 create workflow: got %d want 201", code)
	}
	url := fmt.Sprintf("%s/api/workflows/%d", e.srv.URL, int(body["id"].(float64)))
	code, _ = req(t, http.MethodPost, url+"/nodes", h9tok, map[string]any{"employee_id": w1, "x": 1, "y": 1})
	if code != http.StatusForbidden {
		t.Errorf("H9 add W1 node: got %d want 403", code)
	}
}

func TestHumanSchedulePermissions(t *testing.T) {
	e := newEnv(t)
	_, h1tok, _, w1, _, e9, _, _ := humanTopo(t, e)

	code, adminBody := req(t, http.MethodPost, e.srv.URL+"/api/schedules", adminTok,
		map[string]any{"name": "e9-sc", "employee_id": e9, "every_seconds": 3600, "prompt": "x"})
	if code != http.StatusCreated {
		t.Fatalf("admin create schedule: got %d want 201", code)
	}
	e9ScID := int(adminBody["id"].(float64))

	code, h1Body := req(t, http.MethodPost, e.srv.URL+"/api/schedules", h1tok,
		map[string]any{"name": "w1-sc", "employee_id": w1, "every_seconds": 3600, "prompt": "x"})
	if code != http.StatusCreated {
		t.Fatalf("H1 create schedule for W1: got %d want 201", code)
	}
	w1ScID := int(h1Body["id"].(float64))

	code, _ = req(t, http.MethodPost, e.srv.URL+"/api/schedules", h1tok,
		map[string]any{"name": "e9-sc2", "employee_id": e9, "every_seconds": 3600, "prompt": "x"})
	if code != http.StatusForbidden {
		t.Errorf("H1 create schedule for E9: got %d want 403", code)
	}

	code, body := req(t, http.MethodGet, e.srv.URL+"/api/schedules", h1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("H1 list schedules: got %d want 200", code)
	}
	list, _ := body["schedules"].([]any)
	seen := map[int]bool{}
	for _, v := range list {
		sc := v.(map[string]any)
		seen[int(sc["id"].(float64))] = true
	}
	if !seen[w1ScID] {
		t.Errorf("H1 schedule list missing own schedule %d: %v", w1ScID, seen)
	}
	if seen[e9ScID] {
		t.Errorf("H1 schedule list leaked non-subordinate schedule %d: %v", e9ScID, seen)
	}

	code, _ = req(t, http.MethodPatch, fmt.Sprintf("%s/api/schedules/%d", e.srv.URL, w1ScID),
		h1tok, map[string]any{"enabled": false})
	if code != http.StatusOK {
		t.Errorf("H1 patch own schedule: got %d want 200", code)
	}
	code, _ = req(t, http.MethodPatch, fmt.Sprintf("%s/api/schedules/%d", e.srv.URL, e9ScID),
		h1tok, map[string]any{"enabled": false})
	if code != http.StatusForbidden {
		t.Errorf("H1 patch E9 schedule: got %d want 403", code)
	}

	code, _ = req(t, http.MethodDelete, fmt.Sprintf("%s/api/schedules/%d", e.srv.URL, w1ScID), h1tok, nil)
	if code != http.StatusOK {
		t.Errorf("H1 delete own schedule: got %d want 200", code)
	}
	code, _ = req(t, http.MethodDelete, fmt.Sprintf("%s/api/schedules/%d", e.srv.URL, e9ScID), h1tok, nil)
	if code != http.StatusForbidden {
		t.Errorf("H1 delete E9 schedule: got %d want 403", code)
	}
}

// TestEmptyClosureScheduleList guards against the nil-Down-map leak: callers
// with an empty subordinate closure (leaf humans, AI employees) must receive
// an empty schedule list, never the full set. Regression for the reviewer
// blocker found in the Route B red-team review.
func TestEmptyClosureScheduleList(t *testing.T) {
	e := newEnv(t)
	_, h1tok, _, w1, _, e9, _, h9tok := humanTopo(t, e)

	// Seed schedules: one for W1 (H1's subordinate), one for E9 (nobody's subordinate
	// that matters to H9/E9).
	_, body := req(t, http.MethodPost, e.srv.URL+"/api/schedules", adminTok,
		map[string]any{"name": "w1-sc", "employee_id": w1, "every_seconds": 3600, "prompt": "x"})
	if body["id"] == nil {
		t.Fatalf("seed w1-sc failed")
	}
	_, body = req(t, http.MethodPost, e.srv.URL+"/api/schedules", adminTok,
		map[string]any{"name": "e9-sc", "employee_id": e9, "every_seconds": 3600, "prompt": "x"})
	if body["id"] == nil {
		t.Fatalf("seed e9-sc failed")
	}

	// H9 (human, no subordinates) must see zero schedules.
	code, body := req(t, http.MethodGet, e.srv.URL+"/api/schedules", h9tok, nil)
	if code != http.StatusOK {
		t.Fatalf("H9 list schedules: got %d want 200", code)
	}
	if list, _ := body["schedules"].([]any); len(list) != 0 {
		t.Errorf("H9 schedule list leaked %d rows, want 0: %v", len(list), list)
	}

	// E9 (AI, no subordinates) must also see zero — GET is open to any auth caller,
	// so the filter must not leak the full set.
	e9tok := "tok-E9"
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/schedules", e9tok, nil)
	if code != http.StatusOK {
		t.Fatalf("E9 list schedules: got %d want 200", code)
	}
	if list, _ := body["schedules"].([]any); len(list) != 0 {
		t.Errorf("E9 schedule list leaked %d rows, want 0: %v", len(list), list)
	}

	// Control: H1 (has subordinates incl. W1) still sees the W1 schedule.
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/schedules", h1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("H1 list schedules: got %d want 200", code)
	}
	list, _ := body["schedules"].([]any)
	foundW1 := false
	for _, v := range list {
		sc := v.(map[string]any)
		if int(sc["employee_id"].(float64)) == w1 {
			foundW1 = true
		}
	}
	if !foundW1 {
		t.Errorf("H1 schedule list missing W1 schedule: %v", list)
	}
}

// TestMeSubordinatesAIEmployee verifies the AI /api/me contract: subordinates
// must be an empty array (not a populated closure) since AI cannot manage
// subordinates and adminOrHuman blocks their writes.
func TestMeSubordinatesAIEmployee(t *testing.T) {
	e := newEnv(t)
	_, _, _, _, _, _, _, _ = humanTopo(t, e)

	// L2 is the AI leader of dept B with W1/W2 as subordinates.
	// /api/me for L2 must return subordinates = [] (AI is non-managing).
	l2tok := "tok-L2"
	code, body := req(t, http.MethodGet, e.srv.URL+"/api/me", l2tok, nil)
	if code != http.StatusOK {
		t.Fatalf("L2 me: got %d want 200", code)
	}
	sub := subSet(body, "subordinates")
	if len(sub) != 0 {
		t.Errorf("L2 (AI) subordinates = %v, want empty (AI never manages)", sub)
	}
}
