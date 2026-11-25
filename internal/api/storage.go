package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// Disk-management defaults. loadStorageConfig falls back to these when a
// setting is absent, so a pre-disk-management database keeps today's
// 10 MiB per-file behavior; the 10 GiB quota and 30-day retention are new
// guards that are active by default.
const (
	defaultMaxFileBytes  = 10 << 20 // per-file cap (pre-existing behavior)
	defaultStorageQuota  = 10 << 30 // total storage quota in bytes (0 = unlimited)
	defaultRetentionDays = 30       // artifact retention in days (0 = disabled)
)

// Validation bounds for POST /api/settings.
const (
	minMaxFileBytes   = 1 << 20 // 1 MiB
	maxMaxFileBytes   = 1 << 30 // 1 GiB
	maxStorageQuota   = 1 << 40 // 1 TiB
	maxRetentionDays  = 3650    // 10 years
	maxStoragePathLen = 512     // storage_path length in runes
)

// storageConfig is the effective storage policy at request time.
type storageConfig struct {
	MaxFileBytes  int64 // per-file upload cap
	QuotaBytes    int64 // total usage cap; 0 disables the quota
	RetentionDays int   // artifact TTL; 0 disables the cleanup step
}

// loadStorageConfig reads the storage_* settings. A missing or malformed
// value falls back to the default (fail-open: a bad stored value must not
// break uploads — the settings API validates on write).
func (s *Server) loadStorageConfig() storageConfig {
	cfg := storageConfig{
		MaxFileBytes:  defaultMaxFileBytes,
		QuotaBytes:    defaultStorageQuota,
		RetentionDays: defaultRetentionDays,
	}
	if v := s.Store.GetSettingDefault("storage_max_file_bytes", ""); v != "" {
		// Clamp to the API-validated range so a hand-edited DB value cannot
		// produce a nonsensical cap (review finding #2).
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n < minMaxFileBytes {
				n = minMaxFileBytes
			}
			if n > maxMaxFileBytes {
				n = maxMaxFileBytes
			}
			cfg.MaxFileBytes = n
		}
	}
	if v := s.Store.GetSettingDefault("storage_max_bytes", ""); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.QuotaBytes = n
		}
	}
	if v := s.Store.GetSettingDefault("storage_retention_days", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.RetentionDays = n
		}
	}
	return cfg
}

// diskFreeBytes is the platform free-space probe (disk_unix.go /
// disk_windows.go). It is a package-level var so tests can stub it; the real
// filesystem probe is not deterministic.
var diskFreeBytes = diskFree

// checkUpload enforces the disk-management guards before an upload of
// incomingSize bytes is accepted. Order: per-file cap (413), total quota
// (507 "storage quota exceeded"), free disk space (507 "not enough free
// disk space"). A disk-probe failure is logged and skipped (fail-open).
// When incomingSize is negative (unknown part size) the pre-checks are
// skipped and the returned cap still serves as the streaming backstop.
// On a rejected upload the error response has already been written; ok is
// false and callers must return.
func (s *Server) checkUpload(w http.ResponseWriter, kind string, incomingSize int64) (cap int64, ok bool) {
	cfg := s.loadStorageConfig()
	if incomingSize < 0 {
		return cfg.MaxFileBytes, true
	}
	if incomingSize > cfg.MaxFileBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": kind + " exceeds " + humanBytes(cfg.MaxFileBytes) + " limit",
		})
		return 0, false
	}
	if cfg.QuotaBytes > 0 {
		usage, err := s.Store.StorageUsage()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return 0, false
		}
		if usage.TotalBytes()+incomingSize > cfg.QuotaBytes {
			writeJSON(w, http.StatusInsufficientStorage, map[string]any{"error": "storage quota exceeded"})
			return 0, false
		}
	}
	if free, err := diskFreeBytes(s.Store.FilesDir); err != nil {
		slog.Warn("disk free-space probe failed; skipping guard", "dir", s.Store.FilesDir, "err", err)
	} else if free < incomingSize {
		writeJSON(w, http.StatusInsufficientStorage, map[string]any{"error": "not enough free disk space"})
		return 0, false
	}
	return cfg.MaxFileBytes, true
}

// humanBytes renders a byte count in the largest 1024-based unit that keeps
// the value at least 1 (e.g. "10 MiB", "512 B"), for upload-guard messages.
func humanBytes(n int64) string {
	const unit = 1024
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	f := float64(n)
	i := 0
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0") + " " + units[i]
}

// registerStorageRoutes adds the disk-management admin endpoint.
func (s *Server) registerStorageRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/admin/storage", s.adminOnly(s.handleAdminStorage))
}

// handleAdminStorage: GET /api/admin/storage (admin only). Reports the
// effective storage policy, current usage, and free disk space.
// disk_free_bytes is -1 when the platform probe fails (unknown).
func (s *Server) handleAdminStorage(w http.ResponseWriter, r *http.Request) {
	cfg := s.loadStorageConfig()
	usage, err := s.Store.StorageUsage()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	free := int64(-1)
	if f, err := diskFreeBytes(s.Store.FilesDir); err == nil {
		free = f
	} else {
		slog.Warn("disk free-space probe failed", "dir", s.Store.FilesDir, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":            s.Store.FilesDir,
		"used_bytes":      usage.TotalBytes(),
		"quota_bytes":     cfg.QuotaBytes,
		"max_file_bytes":  cfg.MaxFileBytes,
		"retention_days":  cfg.RetentionDays,
		"disk_free_bytes": free,
		"artifacts":       map[string]any{"count": usage.ArtifactsCount, "bytes": usage.ArtifactsBytes},
		"files":           map[string]any{"count": usage.FilesCount, "bytes": usage.FilesBytes},
	})
}
