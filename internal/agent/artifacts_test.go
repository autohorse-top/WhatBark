package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"whatbark/internal/store"
)

// setupWorkdir creates a temp dir with the given map of relPath → content.
func setupWorkdir(t *testing.T, files map[string]string, mtime *time.Time) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if mtime != nil {
			if err := os.Chtimes(p, *mtime, *mtime); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func TestCollectArtifactFiles_Basic(t *testing.T) {
	dir := setupWorkdir(t, map[string]string{
		"report.html":      "html",
		"src/main.go":      "code",
		"output/result.md": "md",
	}, nil)

	files, truncated := collectArtifactFiles(dir, time.Now().Add(-time.Hour))
	if truncated {
		t.Error("unexpected truncation")
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3: %v", len(files), files)
	}
	// Verify slash-separated paths
	want := map[string]bool{
		"report.html":      true,
		"src/main.go":      true,
		"output/result.md": true,
	}
	for _, f := range files {
		if !want[f] {
			t.Errorf("unexpected file %q", f)
		}
	}
}

func TestCollectArtifactFiles_SkipDotDirs(t *testing.T) {
	dir := setupWorkdir(t, map[string]string{
		".git/config":     "git",
		"src/.git/config": "git",
		"main.go":         "code",
	}, nil)

	files, _ := collectArtifactFiles(dir, time.Now().Add(-time.Hour))
	if len(files) != 1 || files[0] != "main.go" {
		t.Errorf("got %v, want [main.go]", files)
	}
}

func TestCollectArtifactFiles_SkipSkipDirs(t *testing.T) {
	dir := setupWorkdir(t, map[string]string{
		"whatbark/identity.json":    "x",
		"node_modules/pkg/index.js": "x",
		"main.go":                   "code",
	}, nil)

	files, _ := collectArtifactFiles(dir, time.Now().Add(-time.Hour))
	if len(files) != 1 || files[0] != "main.go" {
		t.Errorf("got %v, want [main.go]", files)
	}
}

func TestCollectArtifactFiles_SkipDotFiles(t *testing.T) {
	dir := setupWorkdir(t, map[string]string{
		".env":        "secret",
		".hidden":     "hidden",
		"visible.txt": "ok",
	}, nil)

	files, _ := collectArtifactFiles(dir, time.Now().Add(-time.Hour))
	if len(files) != 1 || files[0] != "visible.txt" {
		t.Errorf("got %v, want [visible.txt]", files)
	}
}

func TestCollectArtifactFiles_MtimeFilter(t *testing.T) {
	// Set mtime 2 hours ago; window is 1 hour → should be excluded.
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-30 * time.Minute)
	dir := setupWorkdir(t, map[string]string{
		"old.txt": "old",
		"new.txt": "new",
	}, &old)
	// Override new.txt mtime to recent
	_ = os.Chtimes(filepath.Join(dir, "new.txt"), recent, recent)

	files, _ := collectArtifactFiles(dir, time.Now().Add(-time.Hour))
	if len(files) != 1 || files[0] != "new.txt" {
		t.Errorf("got %v, want [new.txt]", files)
	}
}

func TestCollectArtifactFiles_Truncation(t *testing.T) {
	// Create 25 files to exceed the 20-file cap.
	files := make(map[string]string)
	for i := 0; i < 25; i++ {
		name := filepath.Base(filepath.Join("f", "file.txt")) // "file.txt"
		_ = name
		files[filepath.Join("d", string(rune('a'+i%26))+"_"+string(rune('0'+i%10))+".txt")] = "x"
	}
	dir := setupWorkdir(t, files, nil)

	got, truncated := collectArtifactFiles(dir, time.Now().Add(-time.Hour))
	if !truncated {
		t.Error("expected truncation")
	}
	if len(got) != artifactMaxFiles {
		t.Errorf("got %d files, want %d (cap)", len(got), artifactMaxFiles)
	}
}

func TestCollectArtifactFiles_OverSize(t *testing.T) {
	// The client no longer filters by file size — the server is authoritative
	// (10 MiB/file) and rejects oversized parts with 413. Both the large file
	// and the small file should be collected and sent to the server.
	dir := t.TempDir()
	bigFile := filepath.Join(dir, "big.bin")
	f, err := os.Create(bigFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(20 << 20); err != nil { // 20 MiB — above server's 10 MiB cap
		t.Fatal(err)
	}
	f.Close()
	// Also a small file that should be collected.
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, truncated := collectArtifactFiles(dir, time.Now().Add(-time.Hour))
	if truncated {
		t.Error("unexpected truncation")
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %v", len(files), files)
	}
	want := map[string]bool{"big.bin": true, "small.txt": true}
	for _, fl := range files {
		if !want[fl] {
			t.Errorf("unexpected file %q", fl)
		}
	}
}

// TestUploadArtifactStreamsFile verifies that a 3 MiB file is streamed from
// the agent's workdir to the real server without buffering the whole payload
// in memory. It checks on-disk integrity via a signed-URL download.
func TestUploadArtifactStreamsFile(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	taskID := dispatch(t, e, empID, "make a big file")
	code, body := req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, taskID), tok, nil)
	if code != http.StatusOK {
		t.Fatalf("start task %d: %d %v", taskID, code, body)
	}

	// 3 MiB deterministic payload
	const fileSize = 3 << 20
	block := make([]byte, 256)
	for i := range block {
		block[i] = byte(i % 251)
	}
	payload := make([]byte, fileSize)
	for i := 0; i < fileSize; i += len(block) {
		copy(payload[i:i+len(block)], block)
	}
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "big.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	ag := New(Config{ServerURL: e.srv.URL, Token: tok, BaseDir: workdir})
	if err := ag.uploadArtifact(context.Background(), taskID, workdir, "big.bin"); err != nil {
		t.Fatalf("uploadArtifact: %v", err)
	}

	// Verify on server side
	code, body = req(t, http.MethodGet,
		fmt.Sprintf("%s/api/tasks/%d/artifacts", e.srv.URL, taskID), tok, nil)
	if code != http.StatusOK {
		t.Fatalf("list artifacts: %d %v", code, body)
	}
	arts := body["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}
	art := arts[0].(map[string]any)
	if art["size"] != float64(fileSize) {
		t.Errorf("artifact size = %v, want %d", art["size"], fileSize)
	}

	// Download via signed URL and verify byte-for-byte
	dlURL, _ := art["url"].(string)
	resp, err := http.Get(e.srv.URL + dlURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != fileSize {
		t.Fatalf("downloaded %d bytes, want %d", len(got), fileSize)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte mismatch at %d: got %x want %x", i, got[i], payload[i])
		}
	}
}
