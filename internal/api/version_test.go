package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/websocket"

	"whatbark/internal/store"
)

// reqHeader is req() plus extra request headers (for version-gate tests).
func reqHeader(t *testing.T, method, url, token string, hdrs map[string]string) (int, map[string]any) {
	t.Helper()
	r, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdrs {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if len(buf) > 0 {
		if err := json.Unmarshal(buf, &out); err != nil {
			t.Fatalf("unmarshal %s %s: %v (body=%s)", method, url, err, buf)
		}
	}
	return resp.StatusCode, out
}

// TestAPIInfo (protocol-v2 A1): GET /api/info is public and advertises the
// supported version range, the build version, and the (future) registration
// toggle.
func TestAPIInfo(t *testing.T) {
	e := newEnv(t)
	code, body := req(t, http.MethodGet, e.srv.URL+"/api/info", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/info: status %d, want 200, body=%v", code, body)
	}
	if body["api_min"] != float64(APIVersionMin) || body["api_max"] != float64(APIVersionMax) {
		t.Errorf("version range = %v..%v, want %d..%d", body["api_min"], body["api_max"], APIVersionMin, APIVersionMax)
	}
	if v, _ := body["server_version"].(string); v == "" {
		t.Error("server_version missing or empty")
	}
	if body["registration"] != false {
		t.Errorf("registration = %v, want false (P2 not landed)", body["registration"])
	}
}

// TestAPIVersionGate (protocol-v2 A2): the /api/ chain negotiates by the
// X-Whatbark-Api header. Absent header = legacy v1 = always accepted;
// in-range values pass; out-of-range values get 426 + supported range;
// malformed values get 400.
func TestAPIVersionGate(t *testing.T) {
	e := newEnv(t)
	dept, err := e.st.CreateDepartment("VDEPT", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, tok := makeEmployee(t, e, "VGATE", store.KindAI, dept)
	url := e.srv.URL + "/api/me"

	cases := []struct {
		header  string // "" = no header
		want    int
		wantErr string // "" = none
	}{
		{"", http.StatusOK, ""}, // legacy v1: no header
		{"1", http.StatusOK, ""},
		{"2", http.StatusOK, ""},
		{"0", http.StatusUpgradeRequired, "api.version.mismatch"},
		{"3", http.StatusUpgradeRequired, "api.version.mismatch"},
		{"abc", http.StatusBadRequest, "api.version.malformed"},
	}
	for _, c := range cases {
		t.Run("hdr="+c.header, func(t *testing.T) {
			var hdrs map[string]string
			if c.header != "" {
				hdrs = map[string]string{APIVersionHeader: c.header}
			}
			code, body := reqHeader(t, http.MethodGet, url, tok, hdrs)
			if code != c.want {
				t.Fatalf("status = %d, want %d (body=%v)", code, c.want, body)
			}
			if c.wantErr != "" && body["error"] != c.wantErr {
				t.Errorf("error = %v, want %q (body=%v)", body["error"], c.wantErr, body)
			}
		})
	}

	// A 426 must carry the supported range so a client can recover.
	code, body := reqHeader(t, http.MethodGet, url, tok, map[string]string{APIVersionHeader: "9"})
	if code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", code)
	}
	sup, _ := body["supported"].([]any)
	if len(sup) != 2 {
		t.Errorf("426 body missing supported range: %v", body)
	}
}

// TestWSVersionGate (protocol-v2 A4): the /ws handshake applies the same
// gate before upgrading: legacy (no header) and v2 succeed, an out-of-range
// version is rejected before the upgrade.
func TestWSVersionGate(t *testing.T) {
	e := newEnv(t)
	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws"
	origin := "http://127.0.0.1"

	// Legacy (no version header) still upgrades.
	ws, err := websocket.Dial(wsURL, adminTok, origin)
	if err != nil {
		t.Fatalf("legacy ws dial: %v", err)
	}
	ws.Close()

	// Declared v2 upgrades fine.
	cfg, err := websocket.NewConfig(wsURL, origin)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Protocol = []string{adminTok}
	cfg.Header.Set(APIVersionHeader, "2")
	ws, err = websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("v2 ws dial: %v", err)
	}
	ws.Close()

	// Out-of-range version must be rejected before the upgrade.
	cfg2, err := websocket.NewConfig(wsURL, origin)
	if err != nil {
		t.Fatal(err)
	}
	cfg2.Protocol = []string{adminTok}
	cfg2.Header.Set(APIVersionHeader, "99")
	if ws, err := websocket.DialConfig(cfg2); err == nil {
		ws.Close()
		t.Fatal("ws dial with X-Whatbark-Api: 99 succeeded, want rejection")
	}
}
