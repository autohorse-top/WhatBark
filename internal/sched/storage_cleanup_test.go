package sched

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"whatbark/internal/flow"
	"whatbark/internal/store"
)

// seedExpiredArtifact creates a done task with an artifact row backdated more
// than days in the past; when writeFile is true the on-disk file is created.
func seedExpiredArtifact(t *testing.T, st *store.Store, empID int, days int, writeFile bool) string {
	t.Helper()
	taskID, err := st.CreateTask(empID, store.KindDispatch, "cleanup", nil, nil, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskStatus(taskID, store.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTaskStatus(taskID, store.TaskDone, "done"); err != nil {
		t.Fatal(err)
	}
	stored := "cc_old.html"
	aid, err := st.InsertArtifact(taskID, "old.html", stored, "text/html", 10)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Duration(days*24+1) * time.Hour)
	if _, err := st.DB().Exec(`UPDATE task_artifacts SET created_at = ? WHERE id = ?`,
		old.UTC().Format("2006-01-02 15:04:05"), aid); err != nil {
		t.Fatal(err)
	}
	if writeFile {
		dir := filepath.Join(st.FilesDir, "artifacts")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, stored), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return stored
}

// TestStorageCleanupRetention: an artifact of a done task older than
// storage_retention_days is deleted (row + on-disk file).
func TestStorageCleanupRetention(t *testing.T) {
	st, w := testStore(t)
	fl := flow.New(st, nil, nil)
	sc := New(st, fl)

	stored := seedExpiredArtifact(t, st, w, 40, true) // 40d old > default 30d

	if err := sc.Step(time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, err := st.GetArtifactByStoredName(stored); err != store.ErrNotFound {
		t.Errorf("expired artifact row still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.FilesDir, "artifacts", stored)); !os.IsNotExist(err) {
		t.Errorf("expired artifact file still on disk (err=%v)", err)
	}
}

// TestStorageCleanupDisabled: retention_days=0 disables TTL deletion (rows and
// files survive), but the orphan sweep still runs.
func TestStorageCleanupDisabled(t *testing.T) {
	st, w := testStore(t)
	fl := flow.New(st, nil, nil)
	sc := New(st, fl)

	if err := st.SetSetting("storage_retention_days", "0"); err != nil {
		t.Fatal(err)
	}
	stored := seedExpiredArtifact(t, st, w, 40, true)

	// An unreferenced file older than the 1h guard window.
	dir := filepath.Join(st.FilesDir, "artifacts")
	orphan := filepath.Join(dir, "orphan_x.bin")
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	if err := sc.Step(time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, err := st.GetArtifactByStoredName(stored); err != nil {
		t.Errorf("retention=0 should keep expired artifact row: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.FilesDir, "artifacts", stored)); err != nil {
		t.Errorf("retention=0 should keep artifact file: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan not swept: %v", err)
	}
}

// TestStorageCleanupThrottle: the cleanup runs at most once per hour; the
// expired artifact survives a second tick 30 minutes later and is removed on
// the tick 70 minutes after the first.
func TestStorageCleanupThrottle(t *testing.T) {
	st, w := testStore(t)
	fl := flow.New(st, nil, nil)
	sc := New(st, fl)

	now := time.Now()
	if err := sc.Step(now); err != nil {
		t.Fatal(err)
	}

	stored := seedExpiredArtifact(t, st, w, 40, false)

	if err := sc.Step(now.Add(30 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetArtifactByStoredName(stored); err != nil {
		t.Errorf("cleanup ran inside the 1h throttle window: artifact deleted at +30min")
	}

	if err := sc.Step(now.Add(70 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetArtifactByStoredName(stored); err != store.ErrNotFound {
		t.Errorf("expired artifact survived the +70min tick: %v", err)
	}
}
