package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestArtifactCRUD: insert, list, count, fetch by stored name.
func TestArtifactCRUD(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	taskID, err := s.CreateTask(worker, KindDispatch, "test task", nil, nil, 0, 5, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	aid, err := s.InsertArtifact(taskID, "report.html", "abc123_report.html", "text/html", 1024)
	if err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}
	if aid == 0 {
		t.Fatal("InsertArtifact returned 0 id")
	}

	arts, err := s.ListArtifactsByTask(taskID)
	if err != nil {
		t.Fatalf("ListArtifactsByTask: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}
	if arts[0].Name != "report.html" || arts[0].StoredName != "abc123_report.html" {
		t.Errorf("artifact = %+v, want name report.html / stored abc123_report.html", arts[0])
	}
	if arts[0].Size != 1024 || arts[0].Mime != "text/html" {
		t.Errorf("artifact size/mime = %d/%q, want 1024/text/html", arts[0].Size, arts[0].Mime)
	}

	n, err := s.CountArtifacts(taskID)
	if err != nil {
		t.Fatalf("CountArtifacts: %v", err)
	}
	if n != 1 {
		t.Errorf("CountArtifacts = %d, want 1", n)
	}

	a, err := s.GetArtifactByStoredName("abc123_report.html")
	if err != nil {
		t.Fatalf("GetArtifactByStoredName: %v", err)
	}
	if a.TaskID != taskID || a.ID != aid {
		t.Errorf("got task %d id %d, want task %d id %d", a.TaskID, a.ID, taskID, aid)
	}
}

// TestArtifactInvalidArgs: argument validation and unknown-task rejection.
func TestArtifactInvalidArgs(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	taskID, err := s.CreateTask(worker, KindDispatch, "x", nil, nil, 0, 5, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := s.InsertArtifact(0, "x", "y", "text/plain", 10); err == nil {
		t.Error("expected error for taskID=0")
	}
	if _, err := s.InsertArtifact(taskID, "", "y", "text/plain", 10); err == nil {
		t.Error("expected error for empty name")
	}
	if _, err := s.InsertArtifact(taskID, "x", "", "text/plain", 10); err == nil {
		t.Error("expected error for empty storedName")
	}
	if _, err := s.InsertArtifact(taskID, "x", "y", "text/plain", -1); err == nil {
		t.Error("expected error for negative size")
	}
	if _, err := s.InsertArtifact(99999, "x", "y", "text/plain", 10); err == nil {
		t.Error("expected error for unknown task")
	}
}

// TestTasksBatch: batch fetch by id — unknown ids absent, duplicates
// collapse, empty input yields an empty map (task-duration-artifacts A:
// backs the inbox task_duration annotation).
func TestTasksBatch(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	ids := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		id, err := s.CreateTask(worker, KindDispatch, "task "+string(rune('a'+i)), nil, nil, 0, 5, nil)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		ids = append(ids, id)
	}

	// Empty input.
	got, err := s.Tasks(nil)
	if err != nil {
		t.Fatalf("Tasks(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Tasks(nil) = %d rows, want 0", len(got))
	}

	// Duplicates + one unknown id: unknown must be absent, duplicates collapse.
	got, err = s.Tasks([]int{ids[0], ids[1], ids[0], 99999})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Tasks with dup+unknown = %d rows, want 2", len(got))
	}
	if _, ok := got[99999]; ok {
		t.Error("unknown id present in result")
	}
	if got[ids[2]] != (Task{}) {
		// ids[2] was not requested: must be absent
		t.Errorf("unrequested task %d present", ids[2])
	}
	if got[ids[0]].EmpID != worker {
		t.Errorf("task %d emp = %d, want %d", ids[0], got[ids[0]].EmpID, worker)
	}
}

// TestArtifactCascadeDelete: deleting the task row removes its artifacts.
func TestArtifactCascadeDelete(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	taskID, err := s.CreateTask(worker, KindDispatch, "cascade test", nil, nil, 0, 5, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := s.InsertArtifact(taskID, "a.txt", "aa1_a.txt", "text/plain", 10); err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}
	if _, err := s.db.Exec("DELETE FROM tasks WHERE id = ?", taskID); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	n, err := s.CountArtifacts(taskID)
	if err != nil {
		t.Fatalf("CountArtifacts after delete: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 artifacts after cascade, got %d", n)
	}
}

// TestStorageUsage: row counts and byte totals for both namespaces.
func TestStorageUsage(t *testing.T) {
	s := openTestStore(t)

	// Empty store: zeros.
	u, err := s.StorageUsage()
	if err != nil {
		t.Fatalf("StorageUsage (empty): %v", err)
	}
	if u.FilesCount != 0 || u.FilesBytes != 0 || u.ArtifactsCount != 0 || u.ArtifactsBytes != 0 || u.TotalBytes() != 0 {
		t.Fatalf("empty usage = %+v, want all zeros", u)
	}

	_, _, _, worker := seedOrg(t, s)
	if _, err := s.InsertFile(0, "a.png", "aa_a.png", "image/png", 100); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	taskID, err := s.CreateTask(worker, KindDispatch, "t", nil, nil, 0, 5, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := s.InsertArtifact(taskID, "r.html", "bb_r.html", "text/html", 200); err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}
	if _, err := s.InsertArtifact(taskID, "r2.html", "cc_r2.html", "text/html", 50); err != nil {
		t.Fatalf("InsertArtifact #2: %v", err)
	}

	u, err = s.StorageUsage()
	if err != nil {
		t.Fatalf("StorageUsage: %v", err)
	}
	if u.FilesCount != 1 || u.FilesBytes != 100 {
		t.Errorf("files = %d/%d bytes, want 1/100", u.FilesCount, u.FilesBytes)
	}
	if u.ArtifactsCount != 2 || u.ArtifactsBytes != 250 {
		t.Errorf("artifacts = %d/%d bytes, want 2/250", u.ArtifactsCount, u.ArtifactsBytes)
	}
	if u.TotalBytes() != 350 {
		t.Errorf("TotalBytes = %d, want 350", u.TotalBytes())
	}
}

// backdateArtifact sets an artifact row's created_at to ts (the store's
// "2006-01-02 15:04:05" UTC format) so retention tests can simulate age.
func backdateArtifact(t *testing.T, s *Store, artID int, ts time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE task_artifacts SET created_at = ? WHERE id = ?`,
		ts.UTC().Format("2006-01-02 15:04:05"), artID); err != nil {
		t.Fatalf("backdate artifact: %v", err)
	}
}

func finishTask(t *testing.T, s *Store, taskID int) {
	t.Helper()
	if err := s.SetTaskStatus(taskID, TaskRunning, ""); err != nil {
		t.Fatalf("start task: %v", err)
	}
	if err := s.SetTaskStatus(taskID, TaskDone, "done"); err != nil {
		t.Fatalf("finish task: %v", err)
	}
}

// TestDeleteExpiredArtifacts: only terminal-state tasks with artifacts older
// than the cutoff are deleted (row + on-disk file); everything else survives.
func TestDeleteExpiredArtifacts(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)
	artDir := filepath.Join(s.FilesDir, "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	old := cutoff.Add(-24 * time.Hour)

	// Case 1: done task, artifact older than cutoff → deleted.
	t1, err := s.CreateTask(worker, KindDispatch, "old done", nil, nil, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := s.InsertArtifact(t1, "old.html", "s1_old.html", "text/html", 10)
	if err != nil {
		t.Fatal(err)
	}
	backdateArtifact(t, s, a1, old)
	if err := s.SetTaskStatus(t1, TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(t1, TaskDone, "done"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "s1_old.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Case 2: running task, artifact older than cutoff → kept (not terminal).
	t2, err := s.CreateTask(worker, KindDispatch, "old running", nil, nil, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.InsertArtifact(t2, "run.html", "s2_run.html", "text/html", 10)
	if err != nil {
		t.Fatal(err)
	}
	backdateArtifact(t, s, a2, old)
	if err := s.SetTaskStatus(t2, TaskRunning, ""); err != nil {
		t.Fatal(err)
	}

	// Case 3: done task, recent artifact → kept (not expired).
	t3, err := s.CreateTask(worker, KindDispatch, "recent done", nil, nil, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.InsertArtifact(t3, "new.html", "s3_new.html", "text/html", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(t3, TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskStatus(t3, TaskDone, "done"); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteExpiredArtifacts(cutoff)
	if err != nil {
		t.Fatalf("DeleteExpiredArtifacts: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}
	if _, err := s.GetArtifactByStoredName("s1_old.html"); !isStoreNotFound(err) {
		t.Errorf("s1 row still present (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(artDir, "s1_old.html")); !os.IsNotExist(err) {
		t.Errorf("s1 file still present on disk")
	}
	if _, err := s.GetArtifactByStoredName("s2_run.html"); err != nil {
		t.Errorf("s2 (running task) row should remain: %v", err)
	}
	if _, err := s.GetArtifactByStoredName("s3_new.html"); err != nil {
		t.Errorf("s3 (recent) row should remain: %v", err)
	}
}

// isStoreNotFound is a local alias so the test file stays self-contained.
func isStoreNotFound(err error) bool { return err == ErrNotFound }

// TestCleanupOrphanArtifacts: files without a task_artifacts row are removed
// only when older than the cutoff; missing dir is not an error.
func TestCleanupOrphanArtifacts(t *testing.T) {
	s := openTestStore(t)

	// Artifacts dir does not exist yet: no error, nothing removed.
	if n, err := s.CleanupOrphanArtifacts(time.Now()); err != nil || n != 0 {
		t.Fatalf("missing dir: n=%d err=%v, want 0/nil", n, err)
	}

	_, _, _, worker := seedOrg(t, s)
	artDir := filepath.Join(s.FilesDir, "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}

	taskID, err := s.CreateTask(worker, KindDispatch, "t", nil, nil, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertArtifact(taskID, "keep.html", "keep_keep.html", "text/html", 10); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-30 * time.Minute)
	files := map[string]time.Time{
		"keep_keep.html": time.Now(), // has a row → kept regardless of mtime
		"orphan_old.bin": old,        // no row, older than cutoff → removed
		"orphan_new.bin": recent,     // no row, inside guard window → kept
	}
	for name, mt := range files {
		p := filepath.Join(artDir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.CleanupOrphanArtifacts(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("CleanupOrphanArtifacts: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed = %d, want 1 (orphan_old only)", n)
	}
	if _, err := os.Stat(filepath.Join(artDir, "orphan_old.bin")); !os.IsNotExist(err) {
		t.Errorf("orphan_old still present (err=%v)", err)
	}
	for _, keep := range []string{"keep_keep.html", "orphan_new.bin"} {
		if _, err := os.Stat(filepath.Join(artDir, keep)); err != nil {
			t.Errorf("%s should be kept: %v", keep, err)
		}
	}
}
