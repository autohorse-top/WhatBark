package api

import (
	"sort"
	"strings"
	"testing"

	"whatbark/internal/store"
	"whatbark/web"
)

// embeddedI18nCodes computes the language set the endpoint must return,
// exactly per the i18n T1 contract: every web/i18n/<lang>.js file, "en"
// always present, "en" first, the rest sorted (deduped).
func embeddedI18nCodes(t *testing.T) []string {
	t.Helper()
	codes := []string{"en"}
	entries, err := web.FS.ReadDir("i18n")
	if err != nil {
		t.Fatalf("read embedded i18n dir: %v", err)
	}
	seen := map[string]bool{"en": true}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		code := strings.TrimSuffix(e.Name(), ".js")
		if !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	sort.Strings(codes[1:])
	return codes
}

// TestI18nLanguages (i18n T1): GET /api/i18n/languages is auth-gated like the
// rest of /api/ and lists the embedded language files — "en" always first.
// The expected set is derived from the build-time embed so the test stays
// green across T1 (en.js only) and T2 (zh.js added): with both files the
// response is {"languages":["en","zh"]}.
func TestI18nLanguages(t *testing.T) {
	e := newEnv(t)

	// Unauthenticated -> 401 (route sits under the /api/ auth middleware).
	if code, _ := req(t, "GET", e.srv.URL+"/api/i18n/languages", "", nil); code != 401 {
		t.Errorf("no token: got %d want 401", code)
	}
	// Bad token -> 401.
	if code, _ := req(t, "GET", e.srv.URL+"/api/i18n/languages", "nope", nil); code != 401 {
		t.Errorf("bad token: got %d want 401", code)
	}

	// Admin token -> 200 with the embedded language set.
	code, body := req(t, "GET", e.srv.URL+"/api/i18n/languages", adminTok, nil)
	if code != 200 {
		t.Fatalf("admin: status %d body %v, want 200", code, body)
	}
	want := embeddedI18nCodes(t)
	raw, _ := body["languages"].([]any)
	got := make([]string, 0, len(raw))
	for _, c := range raw {
		got = append(got, c.(string))
	}
	if len(got) != len(want) {
		t.Fatalf("languages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("languages = %v, want %v", got, want)
		}
	}
	if got[0] != "en" {
		t.Errorf("languages[0] = %q, want \"en\" (always first)", got[0])
	}

	// Any authenticated employee token also gets 200 (not admin-only).
	a, _ := e.st.CreateDepartment("A", nil)
	_, tok := makeEmployee(t, e, "E1", store.KindAI, a)
	if code, _ := req(t, "GET", e.srv.URL+"/api/i18n/languages", tok, nil); code != 200 {
		t.Errorf("employee token: got %d want 200", code)
	}
}
