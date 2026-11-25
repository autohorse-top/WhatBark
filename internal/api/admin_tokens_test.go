package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"whatbark/internal/store"
)

// V-2: multi-admin tokens — lifecycle through the API:
// boot token works, second admin token can be created and authenticates,
// old token keeps working, delete/rotate invalidates, audit records the
// specific admin id.
func TestAdminTokenLifecycle(t *testing.T) {
	e := newEnv(t)
	url := e.srv.URL

	// boot token (seeded by New as "boss") authenticates and exposes admin_id.
	code, body := req(t, http.MethodGet, url+"/api/me", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("me with boot token: status %d body=%v", code, body)
	}
	if body["admin"] != true {
		t.Fatalf("boot token /api/me admin = %v, want true", body["admin"])
	}
	bossID := body["admin_id"].(float64)
	if bossID == 0 {
		t.Fatalf("boot token /api/me admin_id = %v, want > 0", body["admin_id"])
	}
	if body["admin_name"] != "boss" {
		t.Fatalf("boot token /api/me admin_name = %v, want boss", body["admin_name"])
	}

	// employee token cannot manage tokens (403) and anonymous gets 401.
	a, _ := e.st.CreateDepartment("A", nil)
	empID, empTok := makeEmployee(t, e, "worker1", store.KindAI, a)
	_ = empID
	if code, _ := req(t, http.MethodGet, url+"/api/admin/tokens", empTok, nil); code != http.StatusForbidden {
		t.Fatalf("list with employee token: status %d, want 403", code)
	}
	if code, _ := req(t, http.MethodGet, url+"/api/admin/tokens", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("list without token: status %d, want 401", code)
	}

	// create a second admin token.
	code, body = req(t, http.MethodPost, url+"/api/admin/tokens", adminTok, map[string]any{"name": "ops"})
	if code != http.StatusCreated {
		t.Fatalf("create ops token: status %d body=%v", code, body)
	}
	opsTok, _ := body["token"].(string)
	if len(opsTok) != 32 {
		t.Fatalf("ops token len = %d, want 32", len(opsTok))
	}
	opsID := body["id"].(float64)
	if opsID == bossID {
		t.Fatalf("ops token id = %v, want distinct from boss id", opsID)
	}

	// new token authenticates with its own admin identity.
	code, body = req(t, http.MethodGet, url+"/api/me", opsTok, nil)
	if code != http.StatusOK {
		t.Fatalf("me with ops token: status %d body=%v", code, body)
	}
	if body["admin"] != true || body["admin_id"].(float64) != opsID || body["admin_name"] != "ops" {
		t.Fatalf("ops /api/me = %v, want admin with id %v name ops", body, opsID)
	}

	// boot token still works after creating another admin.
	if code, _ := req(t, http.MethodGet, url+"/api/me", adminTok, nil); code != http.StatusOK {
		t.Fatalf("me with boot token after create: status %d", code)
	}

	// list shows both, exposes no secrets.
	code, body = req(t, http.MethodGet, url+"/api/admin/tokens", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("list: status %d body=%v", code, body)
	}
	raw, _ := body["tokens"].([]any)
	if len(raw) != 2 {
		t.Fatalf("list len = %d, want 2", len(raw))
	}
	for _, m := range raw {
		row := m.(map[string]any)
		if _, hasToken := row["token"]; hasToken {
			t.Fatalf("list leaked plaintext token: %v", row)
		}
		if _, hasHash := row["token_hash"]; hasHash {
			t.Fatalf("list leaked token hash: %v", row)
		}
	}

	// validation: bad names and duplicates are 400.
	for _, name := range []string{"", "bad/name", "ops"} {
		if code, _ := req(t, http.MethodPost, url+"/api/admin/tokens", adminTok, map[string]any{"name": name}); code != http.StatusBadRequest {
			t.Fatalf("create %q: status %d, want 400", name, code)
		}
	}

	// unknown id delete is 404.
	if code, _ := req(t, http.MethodDelete, url+"/api/admin/tokens/99999", adminTok, nil); code != http.StatusNotFound {
		t.Fatalf("delete unknown: status %d, want 404", code)
	}

	// audit recorded the create with the specific admin id.
	entries, err := e.st.AuditList(100)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	var found *store.AuditEntry
	for i := range entries {
		if entries[i].Action == "admin.token.create" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no admin.token.create audit entry; got %d entries", len(entries))
	}
	if found.ActorKind != "admin" || found.ActorID != int(bossID) {
		t.Fatalf("audit actor = %s/%d, want admin/%d", found.ActorKind, found.ActorID, int(bossID))
	}
	if !strings.Contains(found.Detail, "name=ops") {
		t.Fatalf("audit detail = %q, want to contain name=ops", found.Detail)
	}

	// rotate: delete the ops token -> immediately invalid.
	code, _ = req(t, http.MethodDelete, fmt.Sprintf("%s/api/admin/tokens/%d", url, int(opsID)), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("delete ops: status %d", code)
	}
	if code, _ := req(t, http.MethodGet, url+"/api/me", opsTok, nil); code != http.StatusUnauthorized {
		t.Fatalf("me with deleted token: status %d, want 401", code)
	}
	code, body = req(t, http.MethodGet, url+"/api/admin/tokens", adminTok, nil)
	raw, _ = body["tokens"].([]any)
	if code != http.StatusOK || len(raw) != 1 {
		t.Fatalf("list after delete: status %d len=%d, want 200/1", code, len(raw))
	}
	if code, _ := req(t, http.MethodGet, url+"/api/me", adminTok, nil); code != http.StatusOK {
		t.Fatalf("me with boot token after delete: status %d, want 200", code)
	}
}
