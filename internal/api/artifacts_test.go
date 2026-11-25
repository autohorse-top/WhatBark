package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"whatbark/internal/store"
)

// multipartPost issues a multipart/form-data POST with one file field plus an
// optional extra form field. Returns (status, response JSON map, headers).
func multipartPost(t *testing.T, url, token, fileField, fileName, content, extraField, extraVal string) (int, map[string]any, http.Header) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if extraField != "" {
		if err := mw.WriteField(extraField, extraVal); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile(fileField, fileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal %s: %v (body=%s)", url, err, raw)
		}
	}
	return resp.StatusCode, out, resp.Header
}

// runningTaskOf dispatches a task to emp (as admin) and starts it with emp's
// token, returning the task id.
func runningTaskOf(t *testing.T, e *env, empID int, tok string) int {
	t.Helper()
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": empID, "kind": "dispatch", "body": "make a report"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: %d %v", code, body)
	}
	taskID := int(body["task_id"].(float64))
	if code, _ := req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, taskID), tok, nil); code != http.StatusOK {
		t.Fatalf("start task %d: %d", taskID, code)
	}
	return taskID
}

// TestTaskArtifactUploadListDownload covers B2 end to end: upload, taskRow
// counts, detail/list endpoints, signed-URL download, auth matrix.
func TestTaskArtifactUploadListDownload(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	l1, l1tok := makeEmployee(t, e, "L1", store.KindAI, a)

	taskID := runningTaskOf(t, e, l1, l1tok)

	// 1) Upload an artifact (multipart: name + file).
	url := fmt.Sprintf("%s/api/agent/tasks/%d/artifacts", e.srv.URL, taskID)
	code, body, hdr := multipartPost(t, url, l1tok, "file", "weather.html", "<html>report</html>", "name", "reports/weather.html")
	if code != http.StatusCreated {
		t.Fatalf("upload: got %d want 201: %v (hdr=%v)", code, body, hdr)
	}
	if body["name"] != "reports/weather.html" {
		t.Errorf("name = %v, want reports/weather.html", body["name"])
	}
	if body["size"] != float64(len("<html>report</html>")) {
		t.Errorf("size = %v", body["size"])
	}
	dlURL, _ := body["url"].(string)
	if !strings.Contains(dlURL, "/api/artifacts/") {
		t.Fatalf("url = %q, want /api/artifacts/...", dlURL)
	}

	// Store row + on-disk file.
	arts, err := e.st.ListArtifactsByTask(taskID)
	if err != nil || len(arts) != 1 {
		t.Fatalf("ListArtifactsByTask = %v err %v", arts, err)
	}
	storedPath := filepath.Join(e.st.FilesDir, "artifacts", filepath.Base(arts[0].StoredName))
	data, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatalf("artifact file on disk: %v", err)
	}
	if string(data) != "<html>report</html>" {
		t.Errorf("on-disk content = %q", data)
	}

	// 2) taskRow carries artifact_count (list) and detail carries artifacts[].
	code, body = req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks?employee=%d", e.srv.URL, l1), l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("list tasks: %d", code)
	}
	rows := body["tasks"].([]any)
	if n := rows[0].(map[string]any)["artifact_count"]; n != float64(1) {
		t.Errorf("artifact_count = %v, want 1", n)
	}
	code, body = req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks/%d", e.srv.URL, taskID), l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("get task: %d", code)
	}
	detailArts := body["artifacts"].([]any)
	if len(detailArts) != 1 {
		t.Fatalf("detail artifacts = %v", detailArts)
	}

	// 3) List endpoint: owner OK with signed URL; other employee 403.
	code, body = req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks/%d/artifacts", e.srv.URL, taskID), l1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("list artifacts: %d %v", code, body)
	}
	listArts := body["artifacts"].([]any)
	listURL := listArts[0].(map[string]any)["url"].(string)
	if !strings.Contains(listURL, "sig=") {
		t.Errorf("signed url missing sig: %q", listURL)
	}
	_, wtok := makeEmployee(t, e, "W", store.KindAI, a)
	if code, _ := req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks/%d/artifacts", e.srv.URL, taskID), wtok, nil); code != http.StatusForbidden {
		t.Errorf("foreign list: got %d want 403", code)
	}

	// 4) Download via the signed URL (no auth header at all).
	resp, err := http.Get(e.srv.URL + listURL)
	if err != nil {
		t.Fatalf("signed GET: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != "<html>report</html>" {
		t.Errorf("signed download: status %d body %q", resp.StatusCode, got)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("html artifact served as %q, want forced octet-stream", ct)
	}

	// 5) Downloads: no credentials 401; valid token 200; unknown name 404.
	resp, err = http.Get(e.srv.URL + "/api/artifacts/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-cred download: got %d want 401", resp.StatusCode)
	}
	getWithToken := func(tok, path string) int {
		t.Helper()
		r, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if c := getWithToken(l1tok, dlURL); c != http.StatusOK {
		t.Errorf("token download: got %d want 200", c)
	}
	if c := getWithToken(l1tok, "/api/artifacts/does_not_exist"); c != http.StatusNotFound {
		t.Errorf("unknown stored name: got %d want 404", c)
	}
}

// TestTaskArtifactGuards covers the upload security/limit matrix.
func TestTaskArtifactGuards(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	l1, l1tok := makeEmployee(t, e, "L1", store.KindAI, a)
	_, wtok := makeEmployee(t, e, "W", store.KindAI, a)

	post := func(tok string, taskID int) int {
		code, _, _ := multipartPost(t,
			fmt.Sprintf("%s/api/agent/tasks/%d/artifacts", e.srv.URL, taskID),
			tok, "file", "a.txt", "x", "name", "a.txt")
		return code
	}

	// Admin may not upload artifacts (agent-only).
	if c := post(adminTok, runningTaskOf(t, e, l1, l1tok)); c != http.StatusForbidden {
		t.Errorf("admin upload: got %d want 403", c)
	}
	// Queued (not running) task rejects artifacts.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": l1, "kind": "dispatch", "body": "later"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: %d", code)
	}
	queuedTask := int(body["task_id"].(float64))
	if c := post(l1tok, queuedTask); c != http.StatusConflict {
		t.Errorf("queued upload: got %d want 409", c)
	}
	// Foreign owner cannot upload to someone else's task.
	otherTask := runningTaskOf(t, e, l1, l1tok)
	if c := post(wtok, otherTask); c != http.StatusForbidden {
		t.Errorf("foreign upload: got %d want 403", c)
	}
	// Unknown task: 404.
	if c := post(l1tok, 99999); c != http.StatusNotFound {
		t.Errorf("unknown task upload: got %d want 404", c)
	}

	// Per-task cap: 20 accepted, 21st rejected.
	capTask := runningTaskOf(t, e, l1, l1tok)
	for i := 0; i < maxArtifactsPerTask; i++ {
		if c := post(l1tok, capTask); c != http.StatusCreated {
			t.Fatalf("upload %d: got %d want 201", i, c)
		}
	}
	if c := post(l1tok, capTask); c != http.StatusConflict {
		t.Errorf("21st upload: got %d want 409", c)
	}
}

// TestTaskArtifactOversize rejects a >10 MiB payload with 413.
func TestTaskArtifactOversize(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	l1, l1tok := makeEmployee(t, e, "L1", store.KindAI, a)
	taskID := runningTaskOf(t, e, l1, l1tok)

	big := strings.Repeat("x", defaultMaxFileBytes+1)
	url := fmt.Sprintf("%s/api/agent/tasks/%d/artifacts", e.srv.URL, taskID)
	code, _, _ := multipartPost(t, url, l1tok, "file", "big.bin", big, "name", "big.bin")
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize upload: got %d want 413", code)
	}
}

// TestTaskArtifactLargeStream verifies a multi-MiB payload streams to disk and
// survives the round trip byte-for-byte. 3 MiB is far past the multipart
// 32 KB spool threshold, so it exercises the part → dest streaming path.
func TestTaskArtifactLargeStream(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	l1, l1tok := makeEmployee(t, e, "L1", store.KindAI, a)
	taskID := runningTaskOf(t, e, l1, l1tok)

	// Deterministic 3 MiB payload: a repeating 256-byte block so any
	// truncation or corruption shows up in the integrity check.
	const want = 3 << 20
	block := make([]byte, 256)
	for i := range block {
		block[i] = byte(i % 251)
	}
	payload := strings.Repeat(string(block), want/len(block))

	url := fmt.Sprintf("%s/api/agent/tasks/%d/artifacts", e.srv.URL, taskID)
	code, body, _ := multipartPost(t, url, l1tok, "file", "big.bin", payload, "name", "data/big.bin")
	if code != http.StatusCreated {
		t.Fatalf("upload 3 MiB: got %d want 201: %v", code, body)
	}
	if body["size"] != float64(want) {
		t.Fatalf("size = %v, want %d", body["size"], want)
	}

	// Download via the signed URL and compare byte-for-byte.
	resp, err := http.Get(e.srv.URL + body["url"].(string))
	if err != nil {
		t.Fatalf("signed GET: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", resp.StatusCode)
	}
	if len(got) != want {
		t.Fatalf("downloaded %d bytes, want %d", len(got), want)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte mismatch at %d: got %x want %x", i, got[i], payload[i])
		}
	}
}

// TestTaskArtifactNameSanitize: path-traversal display names are flattened.
func TestTaskArtifactNameSanitize(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	l1, l1tok := makeEmployee(t, e, "L1", store.KindAI, a)
	taskID := runningTaskOf(t, e, l1, l1tok)

	url := fmt.Sprintf("%s/api/agent/tasks/%d/artifacts", e.srv.URL, taskID)
	code, body, _ := multipartPost(t, url, l1tok, "file", "evil", "x", "name", "../../etc/passwd")
	if code != http.StatusCreated {
		t.Fatalf("upload: %d %v", code, body)
	}
	name, _ := body["name"].(string)
	if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		t.Errorf("sanitized name still hostile: %q", name)
	}
}
