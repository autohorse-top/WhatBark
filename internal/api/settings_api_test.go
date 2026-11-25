package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestSettingsAPI (V-4): admin-only SMTP settings endpoint — unknown key,
// bad port and bad from are 400; a valid set round-trips via GET; the
// password is masked as "<set>" on read and readable as "" after a clear.
func TestSettingsAPI(t *testing.T) {
	e := newEnv(t)

	// Employees cannot read or write settings.
	d, err := e.st.CreateDepartment("D", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, empTok := makeEmployee(t, e, "eve", "ai", d)
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/settings", empTok, nil); code != http.StatusForbidden {
		t.Fatalf("GET /api/settings as employee: status %d, want 403", code)
	}
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", empTok, map[string]string{"smtp_host": "x"}); code != http.StatusForbidden {
		t.Fatalf("POST /api/settings as employee: status %d, want 403", code)
	}

	// Unknown key rejected.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok, map[string]string{"bogus": "x"}); code != http.StatusBadRequest {
		t.Fatalf("unknown key: status %d, want 400", code)
	}
	// Out-of-range port rejected.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok, map[string]string{"smtp_port": "99999"}); code != http.StatusBadRequest {
		t.Fatalf("bad port: status %d, want 400", code)
	}
	// from with two @ rejected.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok, map[string]string{"smtp_from": "a@b@c"}); code != http.StatusBadRequest {
		t.Fatalf("bad from: status %d, want 400", code)
	}
	// Empty body rejected.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok, map[string]string{}); code != http.StatusBadRequest {
		t.Fatalf("empty body: status %d, want 400", code)
	}

	// Valid set round-trips; the password is masked on read.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok, map[string]string{
		"smtp_host": "mail.local", "smtp_port": "2525",
		"smtp_from": "whatbark@local", "smtp_pass": "sekret",
	}); code != http.StatusOK {
		t.Fatalf("set settings: status %d, want 200", code)
	}
	code, body := req(t, http.MethodGet, e.srv.URL+"/api/settings", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("GET settings: status %d", code)
	}
	if body["smtp_host"] != "mail.local" || body["smtp_port"] != "2525" || body["smtp_from"] != "whatbark@local" {
		t.Fatalf("settings roundtrip = %v", body)
	}
	if body["smtp_pass"] != "<set>" {
		t.Fatalf("smtp_pass = %v, want <set>", body["smtp_pass"])
	}

	// Clearing the password stores the empty value; the read is no longer
	// masked (SetSetting upserts, so the key stays present with "").
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok, map[string]string{"smtp_pass": ""}); code != http.StatusOK {
		t.Fatalf("clear smtp_pass: status %d, want 200", code)
	}
	if _, body := req(t, http.MethodGet, e.srv.URL+"/api/settings", adminTok, nil); body["smtp_pass"] != "" {
		t.Fatalf("smtp_pass after clear = %v, want empty", body["smtp_pass"])
	}
}

// TestCreateEmployeeEmail (V-4): the create-employee endpoint accepts an
// optional "email" (persisted and echoed), the list endpoint exposes it, and
// an email over 254 characters is rejected with 400.
func TestCreateEmployeeEmail(t *testing.T) {
	e := newEnv(t)
	d, err := e.st.CreateDepartment("D", nil)
	if err != nil {
		t.Fatal(err)
	}

	code, out := req(t, http.MethodPost, e.srv.URL+"/api/employees", adminTok, map[string]any{
		"name": "mailer", "role": "worker", "kind": "ai", "department_id": d,
		"email": "mailer@example.com",
	})
	if code != http.StatusCreated {
		t.Fatalf("create employee with email: status %d, body %v", code, out)
	}
	if out["email"] != "mailer@example.com" {
		t.Fatalf("create response email = %v", out["email"])
	}

	// The mailbox is visible in the admin list.
	code, list := req(t, http.MethodGet, e.srv.URL+"/api/employees", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("list employees: status %d", code)
	}
	raw, _ := list["employees"].([]any)
	found := false
	for _, row := range raw {
		em, _ := row.(map[string]any)
		if em["name"] == "mailer" {
			found = true
			if em["email"] != "mailer@example.com" {
				t.Fatalf("list email = %v", em["email"])
			}
		}
	}
	if !found {
		t.Fatalf("mailer missing from employee list: %v", list)
	}

	// No email at all still works (empty mailbox).
	if code, out := req(t, http.MethodPost, e.srv.URL+"/api/employees", adminTok, map[string]any{
		"name": "no-mail", "role": "worker", "kind": "ai", "department_id": d,
	}); code != http.StatusCreated || out["email"] != "" {
		t.Fatalf("create without email: status %d body %v", code, out)
	}

	// >254 characters rejected.
	long := strings.Repeat("a", 250) + "@example.com" // 263 chars
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/employees", adminTok, map[string]any{
		"name": "toolong", "role": "worker", "kind": "ai", "department_id": d,
		"email": long,
	}); code != http.StatusBadRequest {
		t.Fatalf("overlong email: status %d, want 400", code)
	}
}

// TestSetGroupPolicyWebhook (V-4): webhook_url alone is a legal policy update
// (previously at least one of priority/sla was required), the value
// round-trips through GET /api/groups/{id}, a non-http(s) scheme is 400, and
// "" clears the URL.
func TestSetGroupPolicyWebhook(t *testing.T) {
	e := newEnv(t)
	d, err := e.st.CreateDepartment("D", nil)
	if err != nil {
		t.Fatal(err)
	}
	empID, _ := makeEmployee(t, e, "gina", "ai", d)

	// Boss creates the group via the API.
	code, gbody := req(t, http.MethodPost, e.srv.URL+"/api/groups", adminTok,
		map[string]any{"name": "hooks", "member_ids": []int{empID}})
	if code != http.StatusCreated {
		t.Fatalf("create group: status %d, body %v", code, gbody)
	}
	gid := int(gbody["id"].(float64))
	policyURL := fmt.Sprintf("%s/api/groups/%d/policy", e.srv.URL, gid)
	getURL := fmt.Sprintf("%s/api/groups/%d", e.srv.URL, gid)

	// Webhook-only update is accepted (previously required priority/sla).
	if code, _ := req(t, http.MethodPost, policyURL, adminTok,
		map[string]any{"webhook_url": "https://hooks.example.com/x"}); code != http.StatusOK {
		t.Fatalf("webhook-only policy update: status %d, want 200", code)
	}

	// Round-trips through the group view.
	if code, g := req(t, http.MethodGet, getURL, adminTok, nil); code != http.StatusOK ||
		g["webhook_url"] != "https://hooks.example.com/x" {
		t.Fatalf("GET group webhook_url = %v (status %d)", g["webhook_url"], code)
	}

	// Non-http(s) scheme rejected.
	if code, _ := req(t, http.MethodPost, policyURL, adminTok,
		map[string]any{"webhook_url": "ftp://nope"}); code != http.StatusBadRequest {
		t.Fatalf("bad scheme: status %d, want 400", code)
	}

	// Empty body still rejected (the at-least-one check now includes the
	// webhook field, but an empty body sets none of them).
	if code, _ := req(t, http.MethodPost, policyURL, adminTok, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("empty policy body: status %d, want 400", code)
	}

	// "" clears the URL.
	code, out := req(t, http.MethodPost, policyURL, adminTok, map[string]any{"webhook_url": ""})
	if code != http.StatusOK || out["webhook_url"] != "" {
		t.Fatalf("clear webhook: status %d, body %v", code, out)
	}
	if code, g := req(t, http.MethodGet, getURL, adminTok, nil); code != http.StatusOK || g["webhook_url"] != "" {
		t.Fatalf("GET after clear: webhook_url = %v (status %d)", g["webhook_url"], code)
	}

	// Unknown group → 404.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/groups/99999/policy", adminTok,
		map[string]any{"webhook_url": "https://x.example"}); code != http.StatusNotFound {
		t.Fatalf("unknown group policy: status %d, want 404", code)
	}
}
