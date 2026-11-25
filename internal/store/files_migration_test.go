package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLegacyStoredNameMigration (F9): pre-F9 stored_name values (spaces,
// parens, '?', '&', ...) break the /api/files URL structure and the signed-URL
// auth boundary. Open rewrites them to "<16-hex>.<ext>" names, renames the
// on-disk file when present, tolerates missing files, and is idempotent.
func TestLegacyStoredNameMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mig.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Legacy row whose file exists on disk (space + parens break the URL).
	legacy := "old report (v1).png"
	id, err := s.InsertFile(0, "old report (v1).png", legacy, "image/png", 4)
	if err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	filesDir := s.FilesDir
	oldPath := filepath.Join(filesDir, "files", legacy)
	if err := os.WriteFile(oldPath, []byte("png"), 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	// Orphan row: no on-disk file — must migrate without error.
	if _, err := s.InsertFile(0, "ghost.txt", "ghost file.bin", "text/plain", 3); err != nil {
		t.Fatalf("insert orphan: %v", err)
	}
	// A row already matching StoredNameRe must stay untouched.
	if _, err := s.InsertFile(0, "safe.png", "deadbeef_safe.png", "image/png", 2); err != nil {
		t.Fatalf("insert safe: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	f, err := s2.GetFile(id)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if f.StoredName == legacy {
		t.Errorf("legacy stored_name not migrated: %q", f.StoredName)
	}
	if !StoredNameRe.MatchString(f.StoredName) {
		t.Errorf("migrated stored_name %q does not match StoredNameRe", f.StoredName)
	}
	if _, err := os.Stat(filepath.Join(filesDir, "files", f.StoredName)); err != nil {
		t.Errorf("migrated on-disk file missing: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old on-disk file %q not renamed (err=%v)", legacy, err)
	}

	// Orphan row: DB migrated, no disk file expected.
	got, err := s2.ListFilesByOwner(0)
	if err != nil {
		t.Fatalf("ListFilesByOwner: %v", err)
	}
	byName := map[string]File{}
	for _, ff := range got {
		byName[ff.Name] = ff
	}
	ghost, ok := byName["ghost.txt"]
	if !ok {
		t.Fatalf("ghost.txt row missing after migration: %v", got)
	}
	if ghost.StoredName == "ghost file.bin" {
		t.Error("orphan legacy stored_name not migrated")
	}
	if !StoredNameRe.MatchString(ghost.StoredName) {
		t.Errorf("orphan migrated stored_name %q invalid", ghost.StoredName)
	}
	if _, err := os.Stat(filepath.Join(filesDir, "files", ghost.StoredName)); !os.IsNotExist(err) {
		t.Errorf("orphan file unexpectedly on disk (err=%v)", err)
	}
	safe, ok := byName["safe.png"]
	if !ok || safe.StoredName != "deadbeef_safe.png" {
		t.Errorf("safe row changed: %+v", safe)
	}

	// Idempotency: a second reopen changes nothing.
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s3, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	t.Cleanup(func() { _ = s3.Close() })
	f3, err := s3.GetFile(id)
	if err != nil {
		t.Fatalf("GetFile 2: %v", err)
	}
	if f3.StoredName != f.StoredName {
		t.Errorf("second reopen changed stored_name %q -> %q", f.StoredName, f3.StoredName)
	}
}
