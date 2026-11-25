package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// writableSettings are the only keys GET/POST /api/settings accept.
var writableSettings = map[string]bool{
	"smtp_host":              true, // V-4
	"smtp_port":              true,
	"smtp_from":              true,
	"smtp_user":              true,
	"smtp_pass":              true,
	"registration_token":     true, // P2: gates POST /api/register
	"storage_max_file_bytes": true, // disk mgmt: per-file upload cap (bytes)
	"storage_max_bytes":      true, // disk mgmt: total storage quota (bytes; 0 = unlimited)
	"storage_retention_days": true, // disk mgmt: artifact retention (days; 0 = off)
	"storage_path":           true, // disk mgmt: FilesDir override (applied at boot)
	// Leader orchestration (S1): review-wake throttle — max kind=review
	// tasks injected per AI requester per hour. 0 disables the wake; non-
	// secret, so GET echoes it verbatim.
	"review_max_per_hour": true,
}

// handleGetSettings: GET /api/settings (admin only, V-4 + P2). Returns the
// SMTP notification settings plus the registration token state; secret values
// (smtp_pass, registration_token) are masked as "<set>" when non-empty so a
// stored secret is never echoed back.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := s.Store.ListSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]any{}
	for k, v := range all {
		if !writableSettings[k] {
			continue
		}
		if (k == "smtp_pass" || k == registrationTokenKey) && v != "" {
			v = "<set>"
		}
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSetSettings: POST /api/settings (admin only, V-4 + P2). Body is a
// subset of writableSettings; unknown keys are 400. An empty string clears the
// key. registration_token must match [A-Za-z0-9_-]{16,128} and is stored as a
// SHA-256 digest (plaintext never persisted). Audit is recorded by the
// adminOnly wrapper.
func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var b map[string]string
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(b) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("at least one setting required"))
		return
	}
	for k, v := range b {
		if !writableSettings[k] {
			writeErr(w, http.StatusBadRequest, errors.New("unknown setting key "+k))
			return
		}
		if k == registrationTokenKey && v != "" && !registrationTokenRe.MatchString(v) {
			writeErr(w, http.StatusBadRequest, errors.New("registration_token must match [A-Za-z0-9_-]{16,128}"))
			return
		}
		if k == "smtp_port" && v != "" {
			p, err := strconv.Atoi(v)
			if err != nil || p < 1 || p > 65535 {
				writeErr(w, http.StatusBadRequest, errors.New("smtp_port must be an integer in [1,65535]"))
				return
			}
		}
		if k == "smtp_from" && v != "" {
			at := 0
			for i := 0; i < len(v); i++ {
				if v[i] == '@' {
					at++
				}
			}
			if at != 1 {
				writeErr(w, http.StatusBadRequest, errors.New("smtp_from must contain exactly one @"))
				return
			}
		}
		if k == "storage_max_file_bytes" && v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < minMaxFileBytes || n > maxMaxFileBytes {
				writeErr(w, http.StatusBadRequest,
					fmt.Errorf("storage_max_file_bytes must be an integer in [%d, %d]", minMaxFileBytes, maxMaxFileBytes))
				return
			}
		}
		if k == "storage_max_bytes" && v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || n > maxStorageQuota {
				writeErr(w, http.StatusBadRequest,
					fmt.Errorf("storage_max_bytes must be an integer in [0, %d]", maxStorageQuota))
				return
			}
		}
		if k == "storage_retention_days" && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > maxRetentionDays {
				writeErr(w, http.StatusBadRequest,
					fmt.Errorf("storage_retention_days must be an integer in [0, %d]", maxRetentionDays))
				return
			}
		}
		if k == "storage_path" && v != "" && len([]rune(v)) > maxStoragePathLen {
			writeErr(w, http.StatusBadRequest,
				fmt.Errorf("storage_path must be at most %d characters", maxStoragePathLen))
			return
		}
		// Leader orchestration (S1): integer in [0,1000]; 0 disables the
		// review wake. Empty string clears the key (flow then applies the
		// default 30).
		if k == "review_max_per_hour" && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > 1000 {
				writeErr(w, http.StatusBadRequest,
					fmt.Errorf("review_max_per_hour must be an integer in [0,1000]"))
				return
			}
		}
	}
	for k, v := range b {
		// P2: registration_token is persisted as a SHA-256 digest; the
		// plaintext token never touches the database (same trust model as
		// employee/admin tokens).
		if k == registrationTokenKey && v != "" {
			sum := sha256.Sum256([]byte(v))
			v = hex.EncodeToString(sum[:])
		}
		if err := s.Store.SetSetting(k, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
