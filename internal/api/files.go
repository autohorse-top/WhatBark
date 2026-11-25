package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"whatbark/internal/hub"
	"whatbark/internal/store"
)

// Body size caps for upload endpoints (bytes). The /api/files per-file cap
// is settings-driven (storage_max_file_bytes, default 10 MiB — see
// storage.go); maxAvatarBytes is fixed by product decision (S12).
const (
	// maxAvatarBytes caps POST /api/me/avatar (S12).
	maxAvatarBytes = 2 << 20
)

// allowed avatar file names: boss.<ext> or emp_<id>.<ext>.
var avatarNameRe = regexp.MustCompile(`^(boss|emp_\d+)\.(png|jpe?g|gif|webp)$`)

// storedNameRe guards /api/files/{name} against traversal or odd names.
// F9: the canonical charset now lives in store.StoredNameRe (shared with the
// startup legacy-name migration); local alias keeps call sites readable.
var storedNameRe = store.StoredNameRe

// imageExtByMime maps detected image MIME types to stored avatar extensions.
var imageExtByMime = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// avatarMimeByExt is the inverse map for serving /api/avatars/{name}.
var avatarMimeByExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// imageKind sniffs the first bytes of data and returns a known image MIME
// type, or "" when the payload is not a known image. Magic bytes, not the
// client-claimed Content-Type, decide.
func imageKind(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 6 && string(data[:6]) == "GIF87a" || len(data) >= 6 && string(data[:6]) == "GIF89a":
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// sanitizeFileName (F9) keeps a display-safe base name: one path element,
// no leading dots, at most 128 runes. Only [A-Za-z0-9._-], Greek letters
// (U+0300–U+036F) and CJK (U+4E00–U+9FFF) survive; everything else —
// spaces, '?', '&', '%', control bytes, other Unicode — becomes '_'. This
// keeps the resulting display/stored names safe to embed in URLs and HTML
// attributes without escaping, so they always satisfy store.StoredNameRe.
func sanitizeFileName(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
			return r
		case r == '.', r == '_', r == '-':
			return r
		case r >= 0x300 && r <= 0x36f, // Greek
			r >= 0x4e00 && r <= 0x9fff: // CJK
			return r
		}
		return '_'
	}, name)
	name = strings.TrimLeft(name, ".")
	if runes := []rune(name); len(runes) > 128 {
		name = string(runes[:128])
	}
	if name == "" {
		name = "file"
	}
	return name
}

// newStoredName returns an unguessable on-disk file name with a 16-hex
// random prefix, retrying on the (astronomically unlikely) collision.
func (s *Server) newStoredName(base string) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("api: generate stored name: %w", err)
		}
		candidate := hex.EncodeToString(buf) + "_" + base
		if _, err := s.Store.GetFileByStoredName(candidate); errors.Is(err, store.ErrNotFound) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("api: check stored name %q: %w", candidate, err)
		}
	}
	return "", errors.New("api: could not allocate a unique stored name")
}

// authIdentity resolves the caller from the Authorization header, falling
// back to a `token` query parameter (P0-1: legacy ?token= is now only a
// back-compat path for old clients; new clients use short-lived signed URLs
// from the server, so full tokens no longer ride on file/avatar URLs).
// F11: the query-param fallback can be cut entirely by setting
// WHATBARK_DISABLE_LEGACY_QUERY_TOKEN=1 (Server.legacyQueryTokenDisabled).
func (s *Server) authIdentity(r *http.Request) (identity, bool) {
	tok := r.Header.Get("Authorization")
	if tok == "" && !s.legacyQueryTokenDisabled {
		tok = r.URL.Query().Get("token")
	}
	return s.tokenIdentity(tok)
}

// handleUploadFile: POST /api/files (multipart field "file").
// Uploads a message attachment owned by the caller (boss uploads as owner 0).
// Per-file cap is settings-driven (storage_max_file_bytes, default 10 MiB).
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)

	part, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `multipart field "file" required`})
		return
	}
	defer part.Close()

	// Disk management: settings-driven per-file cap + total quota +
	// free-disk guard before the body is read (413/507). header.Size < 0
	// (unknown) skips the pre-checks; the limit reader below still bounds
	// the read.
	cap, ok := s.checkUpload(w, "file", header.Size)
	if !ok {
		return
	}

	data, err := io.ReadAll(io.LimitReader(part, cap+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if int64(len(data)) > cap {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "file exceeds " + humanBytes(cap) + " limit"})
		return
	}

	mime := imageKind(data)
	if mime == "" {
		mime = "application/octet-stream"
	}
	name := sanitizeFileName(header.Filename)
	stored, err := s.newStoredName(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dest := filepath.Join(s.Store.FilesDir, "files", stored)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	owner := 0
	if !id.Admin {
		owner = id.EmpID
	}
	fileID, err := s.Store.InsertFile(owner, name, stored, mime, int64(len(data)))
	if err != nil {
		_ = os.Remove(dest)
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":   fileID,
		"name": name,
		"mime": mime,
		"size": len(data),
		"url":  s.signFileURL("/api/files/" + stored),
	})
}

// handleGetFile: GET /api/files/{name} — top-level route (not behind the
// header-only /api/ middleware) so browser <img>/<a> links can fetch it.
// P0-1: auth is Authorization header OR a server-issued short-lived signed
// URL (?exp=&sig=); legacy ?token= still works as a fallback.
func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authIdentity(r); !ok && !s.fileURLAuth(r) {
		_ = s.Store.AuditAppend("anon", 0, "auth.fail", r.URL.Path, "unauthorized file access") // F10
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing or invalid token"})
		return
	}
	name := r.PathValue("name")
	if !storedNameRe.MatchString(name) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	f, err := s.Store.GetFileByStoredName(name)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	p := filepath.Join(s.Store.FilesDir, "files", filepath.Base(f.StoredName))

	w.Header().Set("Cache-Control", "no-store")
	if strings.HasPrefix(f.Mime, "image/") {
		w.Header().Set("Content-Type", f.Mime)
		w.Header().Set("Content-Disposition", "inline")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(f.Name))
	}
	http.ServeFile(w, r, p)
}

// handleUploadAvatar: POST /api/me/avatar (multipart field "avatar").
// Replaces the caller's avatar (employee) or the boss avatar (admin).
func (s *Server) handleUploadAvatar(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)

	part, _, err := r.FormFile("avatar")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `multipart field "avatar" required`})
		return
	}
	defer part.Close()

	data, err := io.ReadAll(io.LimitReader(part, maxAvatarBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(data) > maxAvatarBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "image exceeds 2 MiB limit"})
		return
	}
	mime := imageKind(data)
	if mime == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "must be an image (png, jpeg, gif or webp)"})
		return
	}
	ext := imageExtByMime[mime]

	var ownerID int
	var oldName string
	var target string
	if id.Admin {
		oldName = s.Store.GetSettingDefault("boss_avatar", "")
		target = "boss." + ext
	} else {
		emp, err := s.Store.GetEmployee(id.EmpID)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, err)
			return
		}
		ownerID = emp.ID
		oldName = emp.Avatar
		target = fmt.Sprintf("emp_%d.%s", emp.ID, ext)
	}

	dir := filepath.Join(s.Store.FilesDir, "avatars")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, target), data, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if oldName != "" && oldName != target {
		_ = os.Remove(filepath.Join(dir, filepath.Base(oldName)))
	}

	if id.Admin {
		if err := s.Store.SetSetting("boss_avatar", target); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	} else if err := s.Store.SetAvatar(ownerID, target); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	s.Hub.Broadcast(hub.Event{Type: "employee.avatar", Payload: map[string]any{
		"employee_id": ownerID,
		"avatar":      s.signFileURL("/api/avatars/" + target),
	}})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "avatar": s.signFileURL("/api/avatars/" + target)})
}

// handleGetAvatar: GET /api/avatars/{name} — top-level route; browser
// <img> srcs cannot set headers, so P0-1 serves via short-lived signed URLs
// (?exp=&sig=); header auth and legacy ?token= still work.
func (s *Server) handleGetAvatar(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authIdentity(r); !ok && !s.fileURLAuth(r) {
		_ = s.Store.AuditAppend("anon", 0, "auth.fail", r.URL.Path, "unauthorized avatar access") // F10
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing or invalid token"})
		return
	}
	name := r.PathValue("name")
	if !avatarNameRe.MatchString(name) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	p := filepath.Join(s.Store.FilesDir, "avatars", filepath.Base(name))
	if _, err := os.Stat(p); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", avatarMimeByExt[strings.ToLower(filepath.Ext(name))])
	http.ServeFile(w, r, p)
}
