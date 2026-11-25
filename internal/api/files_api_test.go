package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"whatbark/internal/store"
)

// S12: file upload/download, avatar upload, and file_ids ownership at send.

// 1x1 transparent PNG.
var pngBytes = func() []byte {
	b, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")
	if err != nil {
		panic(err)
	}
	return b
}()

// Minimal JPEG (SOI + JFIF APP1 + EOI): passes the magic-byte sniff and is a
// valid file a browser can render.
var jpgBytes = []byte{
	0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00,
	0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9,
}

// upload POSTs a single multipart file field and returns (status, json, headers).
func upload(t *testing.T, url, token, field, filename string, content []byte) (int, map[string]any, http.Header) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("upload %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal upload response: %v (body=%s)", err, raw)
		}
	}
	return resp.StatusCode, out, resp.Header
}

// getRaw fetches a URL, optionally authenticating via ?token= query (the
// mechanism static <img>/<a> tags use for file/avatar routes).
func getRaw(t *testing.T, url, token string, viaQuery bool) (int, http.Header, []byte) {
	t.Helper()
	if viaQuery {
		url += "?token=" + token
	}
	r, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" && !viaQuery {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// TestFileUploadDownload covers POST /api/files and GET /api/files/{name}.
func TestFileUploadDownload(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	w1, w1tok := makeEmployee(t, e, "W1", store.KindAI, a)

	// Employee uploads a PNG.
	code, body, _ := upload(t, e.srv.URL+"/api/files", w1tok, "file", "shot.png", pngBytes)
	if code != http.StatusCreated {
		t.Fatalf("upload png: got %d want 201: %v", code, body)
	}
	if body["mime"] != "image/png" {
		t.Errorf("mime = %v, want image/png", body["mime"])
	}
	fileID := int(body["id"].(float64))
	url := body["url"].(string)
	if len(url) <= len("/api/files/") || url[:len("/api/files/")] != "/api/files/" {
		t.Fatalf("url = %q", url)
	}
	// P0-1: server-issued URLs are short-lived signed URLs.
	if !strings.Contains(url, "exp=") || !strings.Contains(url, "sig=") {
		t.Fatalf("upload url not signed: %q", url)
	}
	stored := strings.SplitN(url, "?", 2)[0][len("/api/files/"):]
	p := filepath.Join(e.st.FilesDir, "files", stored)
	if fi, err := os.Stat(p); err != nil {
		t.Fatalf("stored file missing on disk: %v", err)
	} else if fi.Size() != int64(len(pngBytes)) {
		t.Errorf("stored size = %d want %d", fi.Size(), len(pngBytes))
	}

	f, err := e.st.GetFile(fileID)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if f.OwnerID != w1 || f.Name != "shot.png" {
		t.Errorf("file owner/name = %d/%q, want %d/shot.png", f.OwnerID, f.Name, w1)
	}

	// Fetch the signed URL with a Bearer header (same-origin JS) and the
	// plain path via legacy ?token= (old clients).
	for i, u := range []string{url, "/api/files/" + stored} {
		c, hdr, got := getRaw(t, e.srv.URL+u, w1tok, i == 1)
		if c != http.StatusOK {
			t.Errorf("fetch mode %d: got %d want 200", i, c)
		}
		if ct := hdr.Get("Content-Type"); ct != "image/png" {
			t.Errorf("fetch mode %d: content-type = %q want image/png", i, ct)
		}
		if !bytes.Equal(got, pngBytes) {
			t.Errorf("fetch mode %d: body mismatch (%d bytes)", i, len(got))
		}
	}

	// P0-1: the signed URL authenticates by itself (bare GET, no token);
	// the plain path with no credentials still 401s. Unknown/bad names -> 404.
	if c, _, _ := getRaw(t, e.srv.URL+url, "", false); c != http.StatusOK {
		t.Errorf("bare signed URL fetch: got %d want 200", c)
	}
	if c, _, _ := getRaw(t, e.srv.URL+"/api/files/"+stored, "", false); c != http.StatusUnauthorized {
		t.Errorf("fetch without token: got %d want 401", c)
	}
	if c, _, _ := getRaw(t, e.srv.URL+"/api/files/deadbeef_x.png", w1tok, true); c != http.StatusNotFound {
		t.Errorf("fetch unknown name: got %d want 404", c)
	}
	if c, _, _ := getRaw(t, e.srv.URL+"/api/files/bad name.txt", w1tok, true); c != http.StatusNotFound {
		t.Errorf("fetch name with space: got %d want 404", c)
	}

	// Boss uploads a text file: served as octet-stream + download attachment.
	code, body, _ = upload(t, e.srv.URL+"/api/files", adminTok, "file", "notes.txt", []byte("hello"))
	if code != http.StatusCreated {
		t.Fatalf("boss upload txt: got %d want 201: %v", code, body)
	}
	if body["mime"] != "application/octet-stream" {
		t.Errorf("txt mime = %v, want application/octet-stream", body["mime"])
	}
	c, hdr, got := getRaw(t, e.srv.URL+body["url"].(string), adminTok, false)
	if c != http.StatusOK {
		t.Fatalf("boss fetch: got %d", c)
	}
	if cd := hdr.Get("Content-Disposition"); cd != `attachment; filename="notes.txt"` {
		t.Errorf("content-disposition = %q", cd)
	}
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("boss fetch body mismatch")
	}

	// Unauthenticated upload -> 401.
	if c, _, _ := upload(t, e.srv.URL+"/api/files", "", "file", "x.bin", []byte("x")); c != http.StatusUnauthorized {
		t.Errorf("upload without token: got %d want 401", c)
	}

	// Oversized upload (10 MiB cap) -> 413.
	big := bytes.Repeat([]byte{'A'}, 10<<20+1)
	if c, _, _ := upload(t, e.srv.URL+"/api/files", adminTok, "file", "big.bin", big); c != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized upload: got %d want 413", c)
	}
}

// TestFileOwnershipInSend pins the file_ids validation in POST /api/messages:
// only the file's owner may attach it; empty body with attachments is valid.
func TestFileOwnershipInSend(t *testing.T) {
	e := newEnv(t)
	l1, l1tok, w1, w1tok := topo(t, e)

	// W1 uploads a file; L1 tries to attach it -> 400 (not the owner).
	upCode, body, _ := upload(t, e.srv.URL+"/api/files", w1tok, "file", "shot.png", pngBytes)
	if upCode != http.StatusCreated {
		t.Fatalf("upload: got %d want 201", upCode)
	}
	fw := int(body["id"].(float64))
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "chat", "body": "x", "file_ids": []any{float64(fw)}}); code != http.StatusBadRequest {
		t.Errorf("cross-owner file_ids: got %d want 400", code)
	}
	// Unknown file id -> 400.
	if code, _ := req(t, http.MethodPost, e.srv.URL+"/api/messages", w1tok,
		map[string]any{"to": l1, "kind": "report", "body": "x", "file_ids": []any{999999}}); code != http.StatusBadRequest {
		t.Errorf("unknown file id: got %d want 400", code)
	}

	// Owner sends the attachment with an empty body -> 201.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", w1tok,
		map[string]any{"to": l1, "kind": "report", "body": "", "file_ids": []any{float64(fw)}})
	if code != http.StatusCreated {
		t.Fatalf("attach send: got %d want 201: %v", code, body)
	}

	// Recipient's inbox shows the message with the file attached.
	var found map[string]any
	for _, m := range inbox(t, e, l1tok, l1) {
		if m["kind"] == store.MsgReport && m["body"] == "" {
			found = m
		}
	}
	if found == nil {
		t.Fatal("attachment-only report not found in L1 inbox")
	}
	files, _ := found["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("message files = %v, want 1", found["files"])
	}
	if files[0].(map[string]any)["name"] != "shot.png" {
		t.Errorf("attached file name = %v", files[0].(map[string]any)["name"])
	}
}

// TestAvatarUpload covers POST /api/me/avatar and GET /api/avatars/{name}
// for both employees and the boss, including format switch cleanup.
func TestAvatarUpload(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	w1, w1tok := makeEmployee(t, e, "W1", store.KindAI, a)
	empName := "emp_" + strconv.Itoa(w1) + ".png"

	// Employee uploads a PNG avatar.
	code, body, _ := upload(t, e.srv.URL+"/api/me/avatar", w1tok, "avatar", "me.png", pngBytes)
	if code != http.StatusOK {
		t.Fatalf("avatar upload: got %d want 200: %v", code, body)
	}
	// P0-1: the response URL is a short-lived signed URL for the avatar path.
	if av := body["avatar"].(string); !strings.HasPrefix(av, "/api/avatars/"+empName+"?exp=") || !strings.Contains(av, "sig=") {
		t.Errorf("avatar = %v, want signed /api/avatars/%s", body["avatar"], empName)
	}
	if _, err := os.Stat(filepath.Join(e.st.FilesDir, "avatars", empName)); err != nil {
		t.Fatalf("avatar missing on disk: %v", err)
	}

	// /api/me exposes the avatar; /api/employees (admin) too.
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/me", w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("me: %d", code)
	}
	emp, _ := body["employee"].(map[string]any)
	if av := emp["avatar"].(string); !strings.HasPrefix(av, "/api/avatars/"+empName+"?exp=") || !strings.Contains(av, "sig=") {
		t.Errorf("me employee.avatar = %v", emp["avatar"])
	}
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/employees", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("employees: %d", code)
	}
	raw, _ := body["employees"].([]any)
	avOK := false
	if len(raw) == 1 {
		if av, _ := raw[0].(map[string]any)["avatar"].(string); strings.HasPrefix(av, "/api/avatars/"+empName+"?exp=") && strings.Contains(av, "sig=") {
			avOK = true
		}
	}
	if len(raw) != 1 || !avOK {
		t.Errorf("employees avatar = %v", body["employees"])
	}

	// Fetch via ?token= (img src path) and with header; no token -> 401;
	// bad names -> 404.
	c, hdr, got := getRaw(t, e.srv.URL+"/api/avatars/"+empName, w1tok, true)
	if c != http.StatusOK {
		t.Fatalf("avatar fetch: got %d", c)
	}
	if ct := hdr.Get("Content-Type"); ct != "image/png" {
		t.Errorf("avatar content-type = %q", ct)
	}
	if !bytes.Equal(got, pngBytes) {
		t.Errorf("avatar body mismatch")
	}
	if c, _, _ := getRaw(t, e.srv.URL+"/api/avatars/"+empName, w1tok, false); c != http.StatusOK {
		t.Errorf("avatar fetch (header): got %d want 200", c)
	}
	if c, _, _ := getRaw(t, e.srv.URL+"/api/avatars/"+empName, "", false); c != http.StatusUnauthorized {
		t.Errorf("avatar fetch no token: got %d want 401", c)
	}
	if c, _, _ := getRaw(t, e.srv.URL+"/api/avatars/emp_1.exe", w1tok, true); c != http.StatusNotFound {
		t.Errorf("bad avatar name: got %d want 404", c)
	}

	// Non-image avatar -> 400.
	if code, _, _ := upload(t, e.srv.URL+"/api/me/avatar", w1tok, "avatar", "x.txt", []byte("not an image")); code != http.StatusBadRequest {
		t.Errorf("non-image avatar: got %d want 400", code)
	}

	// Boss avatar upload + /api/me (admin) reflects it.
	code, body, _ = upload(t, e.srv.URL+"/api/me/avatar", adminTok, "avatar", "boss.png", pngBytes)
	if code != http.StatusOK {
		t.Fatalf("boss avatar upload: got %d want 200: %v", code, body)
	}
	if av := body["avatar"].(string); !strings.HasPrefix(av, "/api/avatars/boss.png?exp=") || !strings.Contains(av, "sig=") {
		t.Errorf("boss avatar = %v", body["avatar"])
	}
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/me", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("admin me: %d", code)
	}
	if av := body["avatar"].(string); !strings.HasPrefix(av, "/api/avatars/boss.png?exp=") || !strings.Contains(av, "sig=") {
		t.Errorf("admin me avatar = %v (code %d)", body["avatar"], code)
	}

	// Format switch: uploading a JPEG replaces the PNG (old file removed).
	code, body, _ = upload(t, e.srv.URL+"/api/me/avatar", w1tok, "avatar", "me.jpg", jpgBytes)
	if code != http.StatusOK {
		t.Fatalf("avatar format switch: got %d want 200: %v", code, body)
	}
	if av := body["avatar"].(string); !strings.HasPrefix(av, "/api/avatars/emp_"+strconv.Itoa(w1)+".jpg?exp=") || !strings.Contains(av, "sig=") {
		t.Errorf("avatar after switch = %v", body["avatar"])
	}
	if _, err := os.Stat(filepath.Join(e.st.FilesDir, "avatars", empName)); !os.IsNotExist(err) {
		t.Errorf("old png avatar not removed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(e.st.FilesDir, "avatars", "emp_"+strconv.Itoa(w1)+".jpg")); err != nil {
		t.Errorf("new jpg avatar missing: %v", err)
	}
}
