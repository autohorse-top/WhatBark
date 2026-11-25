package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// TaskArtifact is a file an agent produced while working on a task. The
// bytes live on disk under Store.FilesDir/artifacts; this row is the index.
// Agents upload artifacts to the server after a successful task tool run
// (api.handleUploadTaskArtifact), so this works even when the agent runs on
// a different machine than the server.
type TaskArtifact struct {
	ID         int
	TaskID     int
	Name       string // display name (relative path inside the agent workdir)
	StoredName string // unique unguessable on-disk file name
	Mime       string
	Size       int64
	CreatedAt  string
}

// InsertArtifact records a new artifact for taskID. The task must exist
// (the FK constraint is the backstop; the explicit check gives a clearer
// error and catches the unknown-task case before the row write).
func (s *Store) InsertArtifact(taskID int, name, storedName, mime string, size int64) (int, error) {
	if taskID <= 0 || name == "" || storedName == "" {
		return 0, fmt.Errorf("store: insert artifact: invalid arguments")
	}
	if size < 0 {
		return 0, fmt.Errorf("store: insert artifact: negative size %d", size)
	}
	if _, err := s.GetTask(taskID); err != nil {
		return 0, fmt.Errorf("store: insert artifact: task: %w", err)
	}
	res, err := s.db.Exec(`
		INSERT INTO task_artifacts (task_id, name, stored_name, mime, size)
		VALUES (?, ?, ?, ?, ?)`, taskID, name, storedName, mime, size)
	if err != nil {
		return 0, fmt.Errorf("store: insert artifact: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert artifact: %w", err)
	}
	return int(id), nil
}

// ListArtifactsByTask returns a task's artifacts in upload order.
func (s *Store) ListArtifactsByTask(taskID int) ([]TaskArtifact, error) {
	rows, err := s.db.Query(`
		SELECT id, task_id, name, stored_name, mime, size, created_at
		FROM task_artifacts WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts %d: %w", taskID, err)
	}
	defer rows.Close()
	var out []TaskArtifact
	for rows.Next() {
		var a TaskArtifact
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Name, &a.StoredName, &a.Mime, &a.Size, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list artifacts %d: %w", taskID, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountArtifacts returns the number of artifacts attached to a task.
func (s *Store) CountArtifacts(taskID int) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM task_artifacts WHERE task_id = ?`, taskID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count artifacts %d: %w", taskID, err)
	}
	return n, nil
}

// GetArtifactByStoredName resolves a signed-URL download request.
func (s *Store) GetArtifactByStoredName(storedName string) (TaskArtifact, error) {
	var a TaskArtifact
	err := s.db.QueryRow(`
		SELECT id, task_id, name, stored_name, mime, size, created_at
		FROM task_artifacts WHERE stored_name = ?`, storedName).
		Scan(&a.ID, &a.TaskID, &a.Name, &a.StoredName, &a.Mime, &a.Size, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, fmt.Errorf("store: get artifact %q: %w", storedName, err)
	}
	return a, nil
}

// StorageUsage aggregates the disk usage of the two storage namespaces:
// message attachments (files) and task artifacts (task_artifacts). Avatars
// are excluded on purpose: each is capped at 2 MiB and their count is bounded
// by the employee count.
type StorageUsage struct {
	FilesCount     int
	FilesBytes     int64
	ArtifactsCount int
	ArtifactsBytes int64
}

// TotalBytes is the quota-relevant total (files + artifacts).
func (u StorageUsage) TotalBytes() int64 { return u.FilesBytes + u.ArtifactsBytes }

// StorageUsage returns row counts and byte totals for both storage namespaces.
func (s *Store) StorageUsage() (StorageUsage, error) {
	var u StorageUsage
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files`).
		Scan(&u.FilesCount, &u.FilesBytes); err != nil {
		return u, fmt.Errorf("store: storage usage (files): %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM task_artifacts`).
		Scan(&u.ArtifactsCount, &u.ArtifactsBytes); err != nil {
		return u, fmt.Errorf("store: storage usage (artifacts): %w", err)
	}
	return u, nil
}

// DeleteExpiredArtifacts removes task_artifacts rows created before cutoff
// whose owning task reached a terminal state (done/failed/canceled), and
// deletes the on-disk file FilesDir/artifacts/<stored_name> when present
// (missing files are tolerated). created_at is TEXT in
// "2006-01-02 15:04:05" UTC (SQLite datetime('now')), so the cutoff is
// compared as a formatted string. Returns the number of rows deleted.
func (s *Store) DeleteExpiredArtifacts(cutoff time.Time) (int, error) {
	ts := cutoff.UTC().Format("2006-01-02 15:04:05")
	rows, err := s.db.Query(`
		SELECT a.id, a.stored_name FROM task_artifacts a
		JOIN tasks t ON t.id = a.task_id
		WHERE a.created_at < ? AND t.status IN ('done', 'failed', 'canceled')`, ts)
	if err != nil {
		return 0, fmt.Errorf("store: list expired artifacts: %w", err)
	}
	type expired struct {
		id     int
		stored string
	}
	var pending []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.stored); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: list expired artifacts: %w", err)
		}
		pending = append(pending, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("store: list expired artifacts: %w", err)
	}
	_ = rows.Close()

	n := 0
	for _, e := range pending {
		res, err := s.db.Exec(`DELETE FROM task_artifacts WHERE id = ?`, e.id)
		if err != nil {
			slog.Warn("storage cleanup: delete artifact row", "id", e.id, "err", err)
			continue
		}
		if aff, _ := res.RowsAffected(); aff > 0 {
			n++
		}
		// filepath.Base guards against any stored_name that ever carried a
		// path separator; only files directly under artifacts/ are removed.
		if err := os.Remove(filepath.Join(s.FilesDir, "artifacts", filepath.Base(e.stored))); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("storage cleanup: remove artifact file", "stored_name", e.stored, "err", err)
		}
	}
	return n, nil
}

// CleanupOrphanArtifacts removes files under FilesDir/artifacts that have no
// task_artifacts row and whose mtime is before cutoff. The guard window
// (callers pass now-1h) protects in-flight uploads. A missing artifacts
// directory is not an error. Returns the number of files removed.
func (s *Store) CleanupOrphanArtifacts(cutoff time.Time) (int, error) {
	dir := filepath.Join(s.FilesDir, "artifacts")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read artifacts dir: %w", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().After(cutoff) {
			continue // too recent: probably an in-flight upload
		}
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM task_artifacts WHERE stored_name = ?`, e.Name()).
			Scan(&count); err != nil {
			slog.Warn("storage cleanup: orphan lookup", "file", e.Name(), "err", err)
			continue
		}
		if count > 0 {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("storage cleanup: remove orphan file", "file", e.Name(), "err", err)
			continue
		}
		n++
	}
	return n, nil
}
