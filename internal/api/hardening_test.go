package api

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"whatbark/internal/store"
)

// TestSanitizeFileName (F9): upload display/stored base names keep only
// URL/HTML-safe characters — [A-Za-z0-9._-], Greek U+0300–036F, CJK
// U+4E00–9FFF; everything else becomes '_'. Leading dots are stripped, the
// name is capped at 128 runes, and the empty result falls back to "file".
func TestSanitizeFileName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"report.png", "report.png"},
		{`a"b?x#.png`, "a_b_x_.png"},
		{"evil.png?sig=1&x=2", "evil.png_sig_1_x_2"},
		{"../../etc/passwd", "passwd"},
		{".hidden", "hidden"},
		{"..", "file"},
		{"", "file"},
		{"报告.png", "报告.png"},
		{"café.txt", "caf_.txt"},
		{"emoji 🚀.png", "emoji__.png"},
		{strings.Repeat("a", 128) + ".png", strings.Repeat("a", 128)},
		{strings.Repeat("报", 130), strings.Repeat("报", 128)},
	}
	for _, c := range cases {
		if got := sanitizeFileName(c.in); got != c.want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUploadDangerousName (F9): a client-supplied filename that carries a
// query-string payload is sanitized so the returned URL cannot break its
// structure or inject into HTML attributes.
func TestUploadDangerousName(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	_, w1tok := makeEmployee(t, e, "D1", store.KindAI, a)

	code, body, _ := upload(t, e.srv.URL+"/api/files", w1tok, "file", "evil.png?sig=1&x=2", pngBytes)
	if code != http.StatusCreated {
		t.Fatalf("upload: got %d want 201: %v", code, body)
	}
	if body["name"] != "evil.png_sig_1_x_2" {
		t.Errorf("display name = %v, want evil.png_sig_1_x_2", body["name"])
	}
	signed := body["url"].(string)
	plain := strings.SplitN(signed, "?", 2)[0]
	stored := strings.TrimPrefix(plain, "/api/files/")
	if !store.StoredNameRe.MatchString(stored) {
		t.Errorf("stored name %q violates StoredNameRe", stored)
	}
	// The signed URL serves the uploaded bytes without any auth header.
	if c, _, got := getRaw(t, e.srv.URL+signed, "", false); c != http.StatusOK || !bytes.Equal(got, pngBytes) {
		t.Errorf("signed fetch: code %d want 200, got %d bytes", c, len(got))
	}
	// The sanitized display name itself is not a stored file (on-disk names
	// carry a random 16-hex prefix), so the payload is inert.
	if c, _, _ := getRaw(t, e.srv.URL+"/api/files/evil.png_sig_1_x_2", w1tok, true); c != http.StatusNotFound {
		t.Errorf("fetch display name: got %d want 404", c)
	}
}

// TestUploadCJKRoundTrip (F9): CJK display names survive the round trip.
func TestUploadCJKRoundTrip(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	_, w1tok := makeEmployee(t, e, "C1", store.KindAI, a)

	code, body, _ := upload(t, e.srv.URL+"/api/files", w1tok, "file", "报告.png", pngBytes)
	if code != http.StatusCreated {
		t.Fatalf("upload: got %d want 201: %v", code, body)
	}
	if body["name"] != "报告.png" {
		t.Errorf("display name = %v, want 报告.png", body["name"])
	}
	signed := body["url"].(string)
	stored := strings.TrimPrefix(strings.SplitN(signed, "?", 2)[0], "/api/files/")
	if !store.StoredNameRe.MatchString(stored) {
		t.Errorf("stored name %q violates StoredNameRe", stored)
	}
	if _, err := os.Stat(filepath.Join(e.st.FilesDir, "files", stored)); err != nil {
		t.Errorf("stored file missing on disk: %v", err)
	}
	if c, _, got := getRaw(t, e.srv.URL+signed, "", false); c != http.StatusOK || !bytes.Equal(got, pngBytes) {
		t.Errorf("signed fetch: code %d want 200", c)
	}
}

// TestFileURLTTLClamp (F7): a configured TTL beyond the 24h acceptance window
// is clamped; non-positive values fall back to the 1h default.
func TestFileURLTTLClamp(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, time.Hour},
		{-time.Hour, time.Hour},
		{30 * time.Second, 30 * time.Second},
		{time.Hour, time.Hour},
		{fileURLMaxLifetime, fileURLMaxLifetime},
		{fileURLMaxLifetime * 2, fileURLMaxLifetime},
	}
	for _, c := range cases {
		if got := effectiveFileURLTTL(c.in); got != c.want {
			t.Errorf("effectiveFileURLTTL(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestAuthFailAudit (F10): 401s on the file/avatar routes (no credentials or
// a malformed signed URL) are recorded as auth.fail audit entries.
func TestAuthFailAudit(t *testing.T) {
	e := newEnv(t)

	if c, _, _ := getRaw(t, e.srv.URL+"/api/files/probe.png", "", false); c != http.StatusUnauthorized {
		t.Fatalf("file 401: got %d", c)
	}
	if c, _, _ := getRaw(t, e.srv.URL+"/api/avatars/emp_1.png", "", false); c != http.StatusUnauthorized {
		t.Fatalf("avatar 401: got %d", c)
	}
	if c, _, _ := getRaw(t, e.srv.URL+"/api/files/x.png?exp=1&sig=bad", "", false); c != http.StatusUnauthorized {
		t.Fatalf("bad-sig 401: got %d", c)
	}

	entries, err := e.st.AuditList(100)
	if err != nil {
		t.Fatalf("AuditList: %v", err)
	}
	seen := map[string]bool{
		"/api/files/probe.png":   false,
		"/api/avatars/emp_1.png": false,
		"/api/files/x.png":       false,
	}
	for _, a := range entries {
		if a.Action == "auth.fail" && a.ActorKind == "anon" {
			seen[a.Target] = true
		}
	}
	for target, ok := range seen {
		if !ok {
			t.Errorf("auth.fail audit missing for %s", target)
		}
	}
}

// TestLegacyTokenDisabled (F11): with WHATBARK_DISABLE_LEGACY_QUERY_TOKEN=1
// the ?token= query fallback is cut on both the file/avatar routes and the WS
// handshake; header auth and signed URLs keep working.
func TestLegacyTokenDisabled(t *testing.T) {
	t.Setenv("WHATBARK_DISABLE_LEGACY_QUERY_TOKEN", "1")
	e := newEnv(t)
	if !e.api.legacyQueryTokenDisabled {
		t.Fatal("server did not pick up WHATBARK_DISABLE_LEGACY_QUERY_TOKEN")
	}
	a, _ := e.st.CreateDepartment("A", nil)
	_, w1tok := makeEmployee(t, e, "L1", store.KindAI, a)

	code, body, _ := upload(t, e.srv.URL+"/api/files", w1tok, "file", "shot.png", pngBytes)
	if code != http.StatusCreated {
		t.Fatalf("upload: got %d want 201: %v", code, body)
	}
	signed := body["url"].(string)
	plain := strings.SplitN(signed, "?", 2)[0]

	if c, _, _ := getRaw(t, e.srv.URL+plain, w1tok, true); c != http.StatusUnauthorized {
		t.Errorf("legacy query token on file route: got %d want 401", c)
	}
	if c, _, _ := getRaw(t, e.srv.URL+plain, w1tok, false); c != http.StatusOK {
		t.Errorf("header auth: got %d want 200", c)
	}
	if c, _, _ := getRaw(t, e.srv.URL+signed, "", false); c != http.StatusOK {
		t.Errorf("signed URL: got %d want 200", c)
	}

	wsBase := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws"
	if ws, err := websocket.Dial(wsBase+"?token="+adminTok, "", "http://127.0.0.1"); err == nil {
		ws.Close()
		t.Error("ws legacy query token dialed successfully, want error")
	}
	ws, err := websocket.Dial(wsBase, adminTok, "http://127.0.0.1")
	if err != nil {
		t.Fatalf("ws subprotocol dial: %v", err)
	}
	ws.Close()
}

// TestMetrics (P1b): /metrics is unauthenticated and exposes coarse counters.
func TestMetrics(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	w1, _ := makeEmployee(t, e, "M1", store.KindAI, a)
	if _, err := e.st.InsertFile(0, "m.png", "0000000000000001_m.png", "image/png", 8); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	if _, err := e.st.CreateTask(w1, "dispatch", "m", nil, nil, 0, 0, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	c, hdr, body := getRaw(t, e.srv.URL+"/metrics", "", false)
	if c != http.StatusOK {
		t.Fatalf("/metrics: got %d want 200", c)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Errorf("content-type = %q", ct)
	}
	s := string(body)
	for _, want := range []string{
		"whatbark_uptime_seconds ",
		"whatbark_goroutines ",
		"whatbark_ws_clients ",
		"whatbark_agents_online ",
		"whatbark_employees_total 1",
		"whatbark_files_total 1",
		`whatbark_tasks_total{status="queued"} 1`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("metrics missing %q in:\n%s", want, s)
		}
	}
}
