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
	"strconv"
	"strings"

	"whatbark/internal/store"
)

// Artifact upload caps (task-duration-artifacts B2 + disk management). The
// server is authoritative: the per-file cap is settings-driven
// (storage_max_file_bytes, default 10 MiB — see storage.go) and 20 files per
// task. The payload is streamed to disk (the multipart parser spools parts
// past 32 KB to an OS temp file, and the handler copies part → dest in 32 KB
// reads), so peak memory is a read buffer, never the file. The agent client
// mirrors the file-count cap only and streams files without a size cap;
// oversized payloads are rejected here with 413.
const (
	maxArtifactsPerTask = 20
)

// executableContentTypes must never be served for task artifacts: an agent
// workdir may contain arbitrary HTML/JS/SVG, and inlining it under the
// whatbark origin would execute it with the user's session (XSS baseline).
// Such types are force-downloaded as octet-stream.
var executableContentTypes = map[string]bool{
	"text/html":              true,
	"text/xml":               true,
	"application/xhtml+xml":  true,
	"image/svg+xml":          true,
	"application/javascript": true,
	"application/xml":        true,
}

// handleUploadTaskArtifact: POST /api/agent/tasks/{id}/artifacts (multipart
// fields "name" = display path, "file" = payload). Agent-only (employee
// token), the task must belong to the caller and be running (artifacts are
// reported before the /finish transition).
//
// The file payload streams straight to the artifacts directory: the
// multipart parser spools parts past 32 KB to an OS temp file, and this
// handler copies part → dest counting bytes against the per-file cap. Peak
// memory is one read buffer, never the whole file.
func (s *Server) handleUploadTaskArtifact(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	if id.Admin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "artifact upload requires an agent (employee) token"})
		return
	}
	taskID, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, ok := s.requireTaskOwner(w, r, taskID)
	if !ok {
		return
	}
	if t.Status != store.TaskRunning {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "task is not running"})
		return
	}
	count, err := s.Store.CountArtifacts(taskID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if count >= maxArtifactsPerTask {
		writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("task artifact limit (%d) reached", maxArtifactsPerTask)})
		return
	}

	name := r.PostFormValue("name")
	part, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `multipart fields "name" and "file" required`})
		return
	}
	defer part.Close()

	// Disk management: settings-driven per-file cap + total quota +
	// free-disk guard before any byte is written (413/507). header.Size < 0
	// (unknown) skips the pre-checks; the streaming cap below still bounds
	// the body.
	cap, ok := s.checkUpload(w, "artifact", header.Size)
	if !ok {
		return
	}

	display := sanitizeArtifactName(name, header.Filename)
	stored, err := s.newArtifactStoredName(display)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	dir := filepath.Join(s.Store.FilesDir, "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dest := filepath.Join(dir, stored)

	// Peek the leading bytes (imageKind needs at most 12) for MIME sniffing,
	// then stream the rest to disk, counting against the per-file cap.
	head := make([]byte, 16)
	hn, _ := part.Read(head)
	out, err := os.Create(dest)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if _, err = out.Write(head[:hn]); err != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	n, cerr := io.CopyN(out, part, cap+1-int64(hn))
	size := int64(hn) + n
	if cerr != nil && !errors.Is(cerr, io.EOF) {
		_ = out.Close()
		_ = os.Remove(dest)
		writeErr(w, http.StatusInternalServerError, cerr)
		return
	}
	if size > cap {
		_ = out.Close()
		_ = os.Remove(dest)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "artifact exceeds " + humanBytes(cap) + " limit"})
		return
	}
	if err = out.Close(); err != nil {
		_ = os.Remove(dest)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	mime := imageKind(head[:hn])
	if mime == "" {
		mime = "application/octet-stream"
	}
	aid, err := s.Store.InsertArtifact(taskID, display, stored, mime, size)
	if err != nil {
		_ = os.Remove(dest)
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":   aid,
		"name": display,
		"size": size,
		"url":  s.signFileURL("/api/artifacts/" + stored),
	})
}

// sanitizeArtifactName keeps a display-safe name (the client sends a relative
// path; every path segment is sanitized so the result is safe in URLs/HTML).
// Falls back to the multipart header filename, then to "artifact".
func sanitizeArtifactName(name, headerFilename string) string {
	if name == "" {
		name = headerFilename
	}
	segs := strings.Split(name, "/")
	for i, seg := range segs {
		seg = sanitizeFileName(seg)
		segs[i] = seg
	}
	out := strings.Join(segs, "/")
	out = strings.TrimLeft(out, "/")
	if out == "" {
		out = "artifact"
	}
	if runes := []rune(out); len(runes) > 200 {
		out = string(runes[:200])
	}
	return out
}

// newArtifactStoredName mints an unguessable on-disk name for the artifacts
// namespace (independent from the message-files namespace).
func (s *Server) newArtifactStoredName(base string) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("api: generate artifact stored name: %w", err)
		}
		candidate := hex.EncodeToString(buf) + "_" + sanitizeFileName(base)
		if !store.StoredNameRe.MatchString(candidate) {
			// sanitizeFileName output always satisfies StoredNameRe for a
			// single segment; a mismatch means the base produced something
			// hostle (defensive, should be unreachable).
			continue
		}
		if _, err := s.Store.GetArtifactByStoredName(candidate); errors.Is(err, store.ErrNotFound) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("api: check artifact stored name %q: %w", candidate, err)
		}
	}
	return "", errors.New("api: could not allocate a unique artifact stored name")
}

// handleListTaskArtifacts: GET /api/tasks/{id}/artifacts (owner or admin).
func (s *Server) handleListTaskArtifacts(w http.ResponseWriter, r *http.Request) {
	taskID, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, ok := s.requireTaskOwner(w, r, taskID); !ok {
		return
	}
	arts, err := s.Store.ListArtifactsByTask(taskID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rows := make([]map[string]any, 0, len(arts))
	for _, a := range arts {
		rows = append(rows, map[string]any{
			"id":         a.ID,
			"name":       a.Name,
			"mime":       a.Mime,
			"size":       a.Size,
			"created_at": a.CreatedAt,
			"url":        s.signFileURL("/api/artifacts/" + a.StoredName),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": rows})
}

// handleGetTaskArtifact: GET /api/authed /api/artifacts/{name} — top-level
// route (outside the header-only /api/ chain) so signed URLs work from plain
// <a href>. Always Content-Disposition: attachment; agent-produced content is
// never inlined in the whatbark origin.
func (s *Server) handleGetTaskArtifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authIdentity(r); !ok && !s.fileURLAuth(r) {
		_ = s.Store.AuditAppend("anon", 0, "auth.fail", r.URL.Path, "unauthorized artifact access")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing or invalid token"})
		return
	}
	name := r.PathValue("name")
	if !store.StoredNameRe.MatchString(name) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	a, err := s.Store.GetArtifactByStoredName(name)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	p := filepath.Join(s.Store.FilesDir, "artifacts", filepath.Base(a.StoredName))
	if _, err := os.Stat(p); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "artifact file missing"})
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	mime := a.Mime
	if executableContentTypes[mime] {
		mime = "application/octet-stream" // never inline executable content
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(a.Name))
	http.ServeFile(w, r, p)
}
