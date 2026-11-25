package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// StoredNameRe is the canonical charset for on-disk stored file names (F9):
// ASCII word characters plus '.', '_' and '-', Greek letters, and CJK.
// The upload path (api.sanitizeFileName + newStoredName) mints names inside
// this charset and the startup migration rewrites pre-F9 rows that fall
// outside it, so the API and the migrator share this single definition.
//
// Note: ' ' (0x20), '?' and every other URL/HTML-hostile byte are rejected —
// stored names must be safe to embed unquoted in URLs and HTML attributes.
var StoredNameRe = regexp.MustCompile(`^[A-Za-z0-9._-\x{300}-\x{36F}\x{4E00}-\x{9FFF}]{1,200}$`)

// File is an uploaded binary (avatar or message attachment). The bytes live
// on disk under Store.FilesDir; this row is the index.
type File struct {
	ID         int
	OwnerID    int
	Name       string // original display name (sanitized)
	StoredName string // unique unguessable on-disk file name
	Mime       string
	Size       int64
	CreatedAt  string
}

// InsertFile records a newly uploaded file owned by employee ownerID.
// ownerID 0 means the boss (admin), who has no employees row.
func (s *Store) InsertFile(ownerID int, name, storedName, mime string, size int64) (int, error) {
	if ownerID < 0 {
		return 0, fmt.Errorf("store: insert file: invalid owner %d", ownerID)
	}
	if ownerID > 0 {
		if _, err := s.GetEmployee(ownerID); err != nil {
			return 0, fmt.Errorf("store: insert file: owner: %w", err)
		}
	}
	if name == "" || storedName == "" {
		return 0, fmt.Errorf("store: insert file: name and stored name are required")
	}
	if size < 0 {
		return 0, fmt.Errorf("store: insert file: negative size %d", size)
	}
	res, err := s.db.Exec(`
		INSERT INTO files (owner_id, name, stored_name, mime, size)
		VALUES (?, ?, ?, ?, ?)`, ownerID, name, storedName, mime, size)
	if err != nil {
		return 0, fmt.Errorf("store: insert file: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert file: %w", err)
	}
	return int(id), nil
}

// GetFile returns a file by id.
func (s *Store) GetFile(id int) (File, error) {
	var f File
	err := s.db.QueryRow(`
		SELECT id, owner_id, name, stored_name, mime, size, created_at
		FROM files WHERE id = ?`, id).
		Scan(&f.ID, &f.OwnerID, &f.Name, &f.StoredName, &f.Mime, &f.Size, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, fmt.Errorf("store: get file %d: %w", id, err)
	}
	return f, nil
}

// GetFileByStoredName returns a file by its on-disk stored name.
func (s *Store) GetFileByStoredName(storedName string) (File, error) {
	var f File
	err := s.db.QueryRow(`
		SELECT id, owner_id, name, stored_name, mime, size, created_at
		FROM files WHERE stored_name = ?`, storedName).
		Scan(&f.ID, &f.OwnerID, &f.Name, &f.StoredName, &f.Mime, &f.Size, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, fmt.Errorf("store: get file by stored name %s: %w", storedName, err)
	}
	return f, nil
}

// ListOwnedFiles returns the given files, but only those owned by ownerID.
// If any id is missing or owned by someone else, ErrNotFound is returned.
// Used to validate a sender's file_ids before linking to a message.
func (s *Store) ListOwnedFiles(ownerID int, ids []int) ([]File, error) {
	out := make([]File, 0, len(ids))
	seen := map[int]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		f, err := s.GetFile(id)
		if err != nil {
			return nil, err
		}
		if f.OwnerID != ownerID {
			return nil, ErrNotFound
		}
		out = append(out, f)
	}
	return out, nil
}

// LinkFilesToMessage associates files with a message (S12 attachments).
func (s *Store) LinkFilesToMessage(messageID int, fileIDs []int) error {
	if len(fileIDs) == 0 {
		return nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, messageID).Scan(&n); err != nil {
		return fmt.Errorf("store: link files to message %d: %w", messageID, err)
	}
	if n == 0 {
		return fmt.Errorf("store: link files to message %d: message not found", messageID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: link files to message %d: %w", messageID, err)
	}
	defer func() { _ = tx.Rollback() }()
	seen := map[int]bool{}
	for _, id := range fileIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.Exec(
			`INSERT INTO message_files (message_id, file_id) VALUES (?, ?)`,
			messageID, id); err != nil {
			return fmt.Errorf("store: link file %d to message %d: %w", id, messageID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: link files to message %d: %w", messageID, err)
	}
	return nil
}

// ListFilesByOwner returns all files owned by ownerID (0 = boss).
func (s *Store) ListFilesByOwner(ownerID int) ([]File, error) {
	rows, err := s.db.Query(`
		SELECT id, owner_id, name, stored_name, mime, size, created_at
		FROM files WHERE owner_id = ? ORDER BY id`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("store: list files of owner %d: %w", ownerID, err)
	}
	defer rows.Close()
	out := make([]File, 0, 8)
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.OwnerID, &f.Name, &f.StoredName, &f.Mime, &f.Size, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list files of owner %d: %w", ownerID, err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list files of owner %d: %w", ownerID, err)
	}
	return out, nil
}

// CountFiles returns the total number of stored files (P1b /metrics).
func (s *Store) CountFiles() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count files: %w", err)
	}
	return n, nil
}

// DeleteFile removes a file row; message_files links cascade.
func (s *Store) DeleteFile(id int) error {
	res, err := s.db.Exec(`DELETE FROM files WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete file %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// migrateLegacyStoredNames (F9) rewrites pre-F9 stored_name values that fall
// outside StoredNameRe (spaces, '?', '&', ... — the old sanitizer kept every
// printable ASCII byte) into a safe "<16-hex>.<ext>" name: the on-disk file
// is renamed when present, the DB row is updated, and a warning is logged.
// Idempotent: rows already matching StoredNameRe are left untouched, so a
// second Open is a no-op. A missing on-disk file is tolerated (DB-only row).
// If the disk rename succeeds but the DB update fails, the rename is rolled
// back (os.Rename newPath -> oldPath) so the row and disk stay consistent;
// if the rollback itself fails both paths are logged for manual repair.
// Every per-row failure leaves the row untouched (logged, not fatal). Called
// by Open after the files layout is prepared.
func migrateLegacyStoredNames(db *sql.DB, filesDir string) error {
	if !tableExists(db, "files") {
		return nil
	}
	rows, err := db.Query(`SELECT id, name, stored_name FROM files ORDER BY id`)
	if err != nil {
		return fmt.Errorf("store: list legacy stored names: %w", err)
	}
	type legacy struct {
		id     int
		name   string
		stored string
	}
	var pending []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.id, &l.name, &l.stored); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: list legacy stored names: %w", err)
		}
		if StoredNameRe.MatchString(l.stored) {
			continue
		}
		pending = append(pending, l)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("store: list legacy stored names: %w", err)
	}
	_ = rows.Close()

	dir := filepath.Join(filesDir, "files")
	for _, l := range pending {
		newName, err := allocateSafeStoredName(db, l.name)
		if err != nil {
			return err
		}
		oldPath := filepath.Join(dir, filepath.Base(l.stored))
		newPath := filepath.Join(dir, newName)
		renamed := false
		if _, err := os.Stat(oldPath); err == nil {
			if err := os.Rename(oldPath, newPath); err != nil {
				slog.Error("F9 stored-name migration: rename failed, row left untouched",
					"file_id", l.id, "old", l.stored, "new", newName, "err", err)
				continue
			}
			renamed = true
		}
		if _, err := db.Exec(`UPDATE files SET stored_name = ? WHERE id = ?`, newName, l.id); err != nil {
			slog.Error("F9 stored-name migration: DB update failed",
				"file_id", l.id, "old", l.stored, "new", newName, "err", err)
			if renamed {
				// The disk file moved but the DB row still points at the old
				// name: roll the rename back so the old name keeps serving
				// and no orphan file is left behind (M2).
				if err := os.Rename(newPath, oldPath); err != nil {
					slog.Error("F9 stored-name migration: rollback failed; manual repair needed",
						"file_id", l.id, "old", oldPath, "new", newPath, "err", err)
				}
			}
			continue
		}
		slog.Warn("F9 legacy stored_name migrated", "file_id", l.id, "old", l.stored, "new", newName)
	}
	return nil
}

// allocateSafeStoredName mints "<16-hex>.<ext>" for a migrated file, where
// ext comes from the original display name (fallback "bin") and the whole
// candidate must pass StoredNameRe. Retries on unique-index collision.
func allocateSafeStoredName(db *sql.DB, originalName string) (string, error) {
	ext := strings.TrimPrefix(filepath.Ext(originalName), ".")
	if ext == "" || !StoredNameRe.MatchString(ext) {
		// No extension (e.g. "deadbeef") or a hostile one: fall back to
		// ".bin" so the stored name never degrades to a trailing dot
		// ("deadbeef.") and always ends in a real, safe extension.
		ext = "bin"
	}
	for attempt := 0; attempt < 8; attempt++ {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("store: generate safe stored name: %w", err)
		}
		candidate := hex.EncodeToString(buf) + "." + ext
		if !StoredNameRe.MatchString(candidate) {
			candidate = hex.EncodeToString(buf) + ".bin"
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE stored_name = ?`, candidate).Scan(&n); err != nil {
			return "", fmt.Errorf("store: check stored name %q: %w", candidate, err)
		}
		if n == 0 {
			return candidate, nil
		}
	}
	return "", errors.New("store: could not allocate a unique safe stored name")
}

// ListMessageFiles returns the files attached to a message, in upload order.
func (s *Store) ListMessageFiles(messageID int) ([]File, error) {
	rows, err := s.db.Query(`
		SELECT f.id, f.owner_id, f.name, f.stored_name, f.mime, f.size, f.created_at
		FROM message_files mf JOIN files f ON f.id = mf.file_id
		WHERE mf.message_id = ?
		ORDER BY f.id`, messageID)
	if err != nil {
		return nil, fmt.Errorf("store: list files of message %d: %w", messageID, err)
	}
	defer rows.Close()
	out := make([]File, 0, 4)
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.OwnerID, &f.Name, &f.StoredName, &f.Mime, &f.Size, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list files of message %d: %w", messageID, err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list files of message %d: %w", messageID, err)
	}
	return out, nil
}
