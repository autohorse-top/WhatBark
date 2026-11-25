package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Artifact upload caps (task-duration-artifacts B3). artifactMaxFiles mirrors
// the server's per-task cap so the agent stops before pointlessly uploading
// files the server will reject with 409. File size is NOT capped here — the
// server is authoritative (10 MiB/file) and rejects oversized parts with 413.
// The agent streams files straight into the request body without buffering.
const (
	artifactMaxFiles    = 20
	artifactGraceWindow = 90 * time.Second
)

// artifactSkipDirs are top-level workdir entries that are never task
// artifacts: platform context dirs, VCS internals, dependency trees.
var artifactSkipDirs = map[string]bool{
	"whatbark":     true, // server-injected context (identity/routing/rules)
	".git":         true,
	"node_modules": true,
}

var errArtifactCap = errors.New("artifact collection cap reached")

// uploadTaskArtifacts walks the employee workdir and uploads regular files
// created or modified during this task (mtime >= task created_at - 90s) as
// task artifacts. It is called after a successful tool run and BEFORE
// finishTask — the server only accepts uploads while the task is running.
//
// Failures are log-only: artifact reporting is an enhancement, never task
// semantics; a flaky upload must not fail the task.
func (a *Agent) uploadTaskArtifacts(ctx context.Context, task Task, workdir string) {
	if workdir == "" {
		return
	}
	since := time.Now().Add(-24 * time.Hour) // conservative default if created_at is missing
	if ts, ok := parseTaskCreatedAt(task.CreatedAt); ok {
		since = ts.Add(-artifactGraceWindow)
	} else {
		slog.Warn("artifact upload: no parseable created_at, using 24h window",
			"task", task.ID, "created_at", task.CreatedAt)
	}

	files, truncated := collectArtifactFiles(workdir, since)
	if len(files) == 0 {
		return
	}
	if truncated {
		slog.Warn("artifact collection truncated by file-count cap", "task", task.ID,
			"collected", len(files))
	}
	okCount := 0
	for _, rel := range files {
		if err := a.uploadArtifact(ctx, task.ID, workdir, rel); err != nil {
			slog.Warn("artifact upload failed", "task", task.ID, "file", rel, "err", err)
			continue
		}
		okCount++
	}
	slog.Info("artifacts uploaded", "task", task.ID, "ok", okCount, "of", len(files))
}

// parseTaskCreatedAt parses the server's created_at (schema-default
// "2006-01-02 15:04:05" UTC; RFC3339 tolerated for forward compat).
func parseTaskCreatedAt(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// collectArtifactFiles walks workdir and returns slash-separated relative
// paths of candidate artifact files. Rules: skip dot entries and skipDirs
// entirely; regular files only; mtime >= since; at most artifactMaxFiles
// files (truncated is the second return value). File size is deliberately
// NOT filtered here — the server is authoritative on size and rejects
// oversized parts with 413.
func collectArtifactFiles(workdir string, since time.Time) (files []string, truncated bool) {
	walkErr := filepath.WalkDir(workdir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip
		}
		rel, err := filepath.Rel(workdir, path)
		if err != nil || rel == "." {
			return nil
		}
		base := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(base, ".") || artifactSkipDirs[base] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(base, ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Before(since) {
			return nil
		}
		if len(files) >= artifactMaxFiles {
			truncated = true
			return errArtifactCap // stop walking
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errArtifactCap) {
		slog.Warn("artifact walk failed", "workdir", workdir, "err", walkErr)
	}
	return files, truncated
}

// uploadArtifact POSTs one workdir file as a multipart task artifact
// (fields: name = relative path for display, file = payload). The file part
// streams directly from disk into the request body via io.MultiReader, so
// only the small multipart framing headers are held in memory regardless of
// file size.
func (a *Agent) uploadArtifact(ctx context.Context, taskID int, workdir, rel string) error {
	abs := filepath.Join(workdir, filepath.FromSlash(rel))
	f, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}

	// Build the multipart framing: the prelude contains everything up to and
	// including the file part header; the file bytes stream from disk in
	// between; the trailer is the closing boundary. Only the framing headers
	// (a few hundred bytes) live in memory.
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if werr := mw.WriteField("name", rel); werr != nil {
		return werr
	}
	fh := make(textproto.MIMEHeader)
	fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filepath.Base(rel)))
	fh.Set("Content-Type", "application/octet-stream")
	fh.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if _, werr := mw.CreatePart(fh); werr != nil {
		return werr
	}
	split := head.Len() // length of prelude (everything before file content)
	if werr := mw.Close(); werr != nil {
		return werr
	}
	hb := head.Bytes()
	body := io.MultiReader(bytes.NewReader(hb[:split]), f, bytes.NewReader(hb[split:]))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.cfg.ServerURL+fmt.Sprintf("/api/agent/tasks/%d/artifacts", taskID), body)
	if err != nil {
		return err
	}
	req.ContentLength = int64(split) + fi.Size() + int64(len(hb)-split)
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set(apiVersionHeader, apiVersion)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &APIError{Status: resp.StatusCode, Body: e.Error}
	}
	return nil
}
