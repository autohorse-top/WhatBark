package api

import (
	"net/http"
	"testing"

	"whatbark/internal/store"
)

// TestReviewSettingsAPI (F5): review_max_per_hour is a writable, non-secret
// setting — a valid value round-trips through GET verbatim, the [0,1000]
// domain is enforced (0 = wake disabled), and junk/out-of-range values are
// 400. Values arrive as strings like every other settings key.
func TestReviewSettingsAPI(t *testing.T) {
	e := newEnv(t)

	// Valid value round-trips in cleartext (not a secret, no masking).
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok,
		map[string]string{"review_max_per_hour": "30"}); code != http.StatusOK {
		t.Fatalf("set 30: status %d, want 200", code)
	}
	code, body := req(t, http.MethodGet, e.srv.URL+"/api/settings", adminTok, nil)
	if code != http.StatusOK || body["review_max_per_hour"] != "30" {
		t.Fatalf("GET after set = %v (status %d), want review_max_per_hour=30 in cleartext", body, code)
	}

	// Domain edges: 0 (disabled) and 1000 accepted.
	for _, good := range []string{"0", "1000"} {
		if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok,
			map[string]string{"review_max_per_hour": good}); code != http.StatusOK {
			t.Errorf("set %s: status %d, want 200", good, code)
		}
	}

	// Out-of-domain and junk values: 400, nothing persisted.
	for _, bad := range []string{"-1", "1001", "x", "1.5"} {
		if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok,
			map[string]string{"review_max_per_hour": bad}); code != http.StatusBadRequest {
			t.Errorf("set %q: status %d, want 400", bad, code)
		}
	}

	// Clearing the key is legal; the upsert leaves the key present with
	// the empty value (same as smtp_pass in TestSettingsAPI), which reads
	// as unset in the flow layer (default 30 then applies).
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok,
		map[string]string{"review_max_per_hour": ""}); code != http.StatusOK {
		t.Fatalf("clear: status %d, want 200", code)
	}
	_, body = req(t, http.MethodGet, e.srv.URL+"/api/settings", adminTok, nil)
	if v := body["review_max_per_hour"]; v != "" {
		t.Fatalf("after clear review_max_per_hour = %q, want \"\"", v)
	}
}

// TestReviewKindSendAPI (F4): kind=review is system-only — employees
// (even department leaders) get 403, while the admin (from=0) can inject a
// review message 201 with the matching review task created (requested_by=0).
func TestReviewKindSendAPI(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	// Employees cannot forge a review task, leader or not. W1 targets his
	// own leader L1; L1 targets W1 — neither may inject a system kind.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", w1tok,
		map[string]any{"to": l1, "kind": "review", "body": "forged"}); code != http.StatusForbidden {
		t.Errorf("plain worker forging review: status %d, want 403", code)
	}
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "review", "body": "forged"}); code != http.StatusForbidden {
		t.Errorf("dept leader forging review: status %d, want 403", code)
	}
	// No review task was created by the attempts.
	if got, err := e.st.CountEmployeeReviewsInHour(w1); err != nil || got != 0 {
		t.Fatalf("reviews after 403s = %d (err %v), want 0", got, err)
	}

	// Admin inject: 201 with message_id + task_id; the task carries the
	// system-only shape.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "review", "body": "Review wake: task #7 by W1 is done."})
	if code != http.StatusCreated {
		t.Fatalf("admin review send: status %d, want 201 (body %v)", code, body)
	}
	taskID := int(body["task_id"].(float64))
	task, err := e.st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Kind != store.KindReview || task.EmpID != l1 || task.RequestedBy != 0 || task.Status != store.TaskQueued {
		t.Errorf("admin-created review task = %+v, want kind=review emp=%d requested_by=0 queued", task, l1)
	}
	msg, err := e.st.GetMessage(int(body["message_id"].(float64)))
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Kind != store.MsgReview || msg.FromID != nil || msg.ToID == nil || *msg.ToID != l1 {
		t.Errorf("admin review message = %+v, want from NULL to=%d", msg, l1)
	}
	if msg.TaskID == nil || *msg.TaskID != taskID {
		t.Errorf("review message task_id = %v, want %d", msg.TaskID, taskID)
	}

	// Unknown kind still falls through to the default 403/400 machinery.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "nope", "body": "x"}); code != http.StatusForbidden {
		t.Errorf("unknown kind: status %d, want 403", code)
	}
}
