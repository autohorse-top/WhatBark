package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// regReq is like req but also sets an extra request header (version-gate
// probing on the public endpoint).
func regReq(t *testing.T, extraHdr string, extraVal string, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if extraHdr != "" {
		r.Header.Set(extraHdr, extraVal)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(buf) > 0 {
		if err := json.Unmarshal(buf, &out); err != nil {
			t.Fatalf("unmarshal %s %s: %v (body=%s)", method, url, err, buf)
		}
	}
	return resp.StatusCode, out
}

const regTok = "reg-token-1234567890" // [A-Za-z0-9_-]{16,128}

// TestRegistration (P2): full lifecycle of POST /api/register.
func TestRegistration(t *testing.T) {
	e := newEnv(t)
	u := e.srv.URL

	deptID, gid, err := e.st.CreateDepartmentWithChannel("Field", nil)
	if err != nil {
		t.Fatalf("seed dept with channel: %v", err)
	}

	info := func() map[string]any {
		code, body := req(t, http.MethodGet, u+"/api/info", "", nil)
		if code != http.StatusOK {
			t.Fatalf("/api/info: status %d", code)
		}
		return body
	}

	// A: disabled by default — discovery reports false, endpoint is 403.
	if info()["registration"] != false {
		t.Fatalf("registration initially = %v, want false", info()["registration"])
	}
	validBody := map[string]any{"token": regTok, "name": "dev-01", "role": "field-sensor", "department_id": deptID}
	code, body := req(t, http.MethodPost, u+"/api/register", "", validBody)
	if code != http.StatusForbidden || body["error"] != "registration.disabled" {
		t.Fatalf("register while disabled: got %d %v, want 403 registration.disabled", code, body)
	}

	// B: token validation on the admin settings endpoint.
	if code, _ = req(t, http.MethodPost, u+"/api/settings", adminTok, map[string]string{"registration_token": "short"}); code != http.StatusBadRequest {
		t.Fatalf("short registration_token: status %d, want 400", code)
	}
	if code, _ = req(t, http.MethodPost, u+"/api/settings", adminTok, map[string]string{"registration_token": "bad!chars@12345678"}); code != http.StatusBadRequest {
		t.Fatalf("bad-charset registration_token: status %d, want 400", code)
	}
	if code, _ = req(t, http.MethodPost, u+"/api/settings", adminTok, map[string]string{"registration_token": regTok}); code != http.StatusOK {
		t.Fatalf("set registration_token: status %d, want 200", code)
	}

	// C: stored hashed (never plaintext), masked in GET, discovery flips.
	stored := e.st.GetSettingDefault("registration_token", "")
	if stored == "" || stored == regTok || len(stored) != 64 {
		t.Fatalf("stored registration_token = %q, want 64-char non-plaintext digest", stored)
	}
	code, body = req(t, http.MethodGet, u+"/api/settings", adminTok, nil)
	if code != http.StatusOK || body["registration_token"] != "<set>" {
		t.Fatalf("GET settings registration_token = %v (status %d), want <set>", body["registration_token"], code)
	}
	if info()["registration"] != true {
		t.Fatalf("registration after enable = %v, want true", info()["registration"])
	}

	// D: wrong/missing token → 401.
	wrong := map[string]any{"token": "wrong-token-12345678", "name": "dev-01", "role": "r", "department_id": deptID}
	if code, body = req(t, http.MethodPost, u+"/api/register", "", wrong); code != http.StatusUnauthorized || body["error"] != "registration.token" {
		t.Fatalf("wrong token: got %d %v, want 401 registration.token", code, body)
	}
	noTok := map[string]any{"name": "dev-01", "role": "r", "department_id": deptID}
	if code, body = req(t, http.MethodPost, u+"/api/register", "", noTok); code != http.StatusUnauthorized || body["error"] != "registration.token" {
		t.Fatalf("missing token: got %d %v, want 401 registration.token", code, body)
	}

	// D2: failed attempts are audited (register.fail) so brute-force probing
	// of the public endpoint is visible to the admin.
	failRows, err := e.st.AuditList(50)
	if err != nil {
		t.Fatalf("AuditList: %v", err)
	}
	var disabledFails, tokenFails int
	for _, row := range failRows {
		if row.Action == "register.fail" && row.ActorKind == "anon" {
			switch row.Detail {
			case "disabled":
				disabledFails++
			case "token":
				tokenFails++
			}
		}
	}
	if disabledFails < 1 || tokenFails < 2 {
		t.Fatalf("register.fail audit rows: disabled=%d (want >=1), token=%d (want >=2)", disabledFails, tokenFails)
	}

	// E: version gate still applies to the public endpoint.
	if code, _ := regReq(t, "X-Whatbark-Api", "99", http.MethodPost, u+"/api/register", validBody); code != http.StatusUpgradeRequired {
		t.Fatalf("register with v=99: status %d, want 426", code)
	}

	// F: happy path — 201, employee token returned once, works on /api/me.
	code, body = req(t, http.MethodPost, u+"/api/register", "", validBody)
	if code != http.StatusCreated {
		t.Fatalf("register: status %d body %v, want 201", code, body)
	}
	newTok, _ := body["token"].(string)
	if newTok == "" {
		t.Fatalf("register: no token in response %v", body)
	}
	if body["kind"] != "ai" {
		t.Fatalf("default kind = %v, want ai", body["kind"])
	}
	newID := int(body["id"].(float64))
	code, me := req(t, http.MethodGet, u+"/api/me", newTok, nil)
	if code != http.StatusOK {
		t.Fatalf("/api/me with registered token: status %d, want 200 (body %v)", code, me)
	}
	emp, _ := me["employee"].(map[string]any)
	if emp == nil || emp["name"] != "dev-01" || emp["kind"] != "ai" || int(emp["id"].(float64)) != newID {
		t.Fatalf("/api/me employee = %v, want dev-01/ai id=%d", me["employee"], newID)
	}

	// F2: auto-joined the department channel (S31 parity with admin create).
	ok, err := e.st.IsGroupMember(gid, newID)
	if err != nil || !ok {
		t.Fatalf("IsGroupMember(channel %d, emp %d) = %v, %v; want true", gid, newID, ok, err)
	}

	// F3: H-7 audit row recorded (fail-soft, but expected here).
	rows, err := e.st.AuditList(5)
	if err != nil {
		t.Fatalf("AuditList: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.Action == "register.create" && row.ActorKind == "employee" && row.ActorID == newID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no register.create audit row (actor employee %d) in %+v", newID, rows)
	}

	// G: duplicate name → 409.
	dup := map[string]any{"token": regTok, "name": "dev-01", "role": "r", "department_id": deptID}
	if code, body = req(t, http.MethodPost, u+"/api/register", "", dup); code != http.StatusConflict || body["error"] != "registration.name_taken" {
		t.Fatalf("duplicate name: got %d %v, want 409 registration.name_taken", code, body)
	}

	// H: field validation.
	badName := map[string]any{"token": regTok, "name": "bad name!", "role": "r", "department_id": deptID}
	if code, _ = req(t, http.MethodPost, u+"/api/register", "", badName); code != http.StatusBadRequest {
		t.Fatalf("bad name: status %d, want 400", code)
	}
	noRole := map[string]any{"token": regTok, "name": "dev-02", "department_id": deptID}
	if code, _ = req(t, http.MethodPost, u+"/api/register", "", noRole); code != http.StatusBadRequest {
		t.Fatalf("missing role: status %d, want 400", code)
	}
	badKind := map[string]any{"token": regTok, "name": "dev-02", "role": "r", "kind": "robot", "department_id": deptID}
	if code, _ = req(t, http.MethodPost, u+"/api/register", "", badKind); code != http.StatusBadRequest {
		t.Fatalf("bad kind: status %d, want 400", code)
	}
	badDept := map[string]any{"token": regTok, "name": "dev-02", "role": "r", "department_id": 99999}
	if code, _ = req(t, http.MethodPost, u+"/api/register", "", badDept); code != http.StatusBadRequest {
		t.Fatalf("unknown department: status %d, want 400", code)
	}
	longEmail := map[string]any{"token": regTok, "name": "dev-02", "role": "r", "department_id": deptID, "email": strings.Repeat("a", 255) + "@x.io"}
	if code, _ = req(t, http.MethodPost, u+"/api/register", "", longEmail); code != http.StatusBadRequest {
		t.Fatalf("email >254: status %d, want 400", code)
	}
	// kind=human and a 254-char email are accepted.
	edge := map[string]any{"token": regTok, "name": "dev-02", "role": "r", "kind": "human", "department_id": deptID, "email": strings.Repeat("a", 240) + "@x.io"}
	if code, body = req(t, http.MethodPost, u+"/api/register", "", edge); code != http.StatusCreated || body["kind"] != "human" {
		t.Fatalf("edge register: %d %v, want 201 kind=human", code, body)
	}

	// I: disabling again turns the endpoint off.
	if code, _ = req(t, http.MethodPost, u+"/api/settings", adminTok, map[string]string{"registration_token": ""}); code != http.StatusOK {
		t.Fatalf("clear registration_token: status %d, want 200", code)
	}
	if info()["registration"] != false {
		t.Fatalf("registration after disable = %v, want false", info()["registration"])
	}
	if code, _ = req(t, http.MethodPost, u+"/api/register", "", validBody); code != http.StatusForbidden {
		t.Fatalf("register after disable: status %d, want 403", code)
	}
}
