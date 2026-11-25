package api

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"whatbark/internal/store"
)

// TestStorageSettingsValidation pins the POST /api/settings range checks for
// the disk-management keys: out-of-range or malformed values 400, in-range
// values persist, and clearing a key works.
func TestStorageSettingsValidation(t *testing.T) {
	e := newEnv(t)

	set := func(v map[string]string) int {
		t.Helper()
		code, _ := req(t, http.MethodPost, e.srv.URL+"/api/settings", adminTok, v)
		return code
	}

	cases := []struct {
		body map[string]string
		want int
	}{
		{map[string]string{"storage_max_file_bytes": "1048576"}, http.StatusOK},    // 1 MiB min
		{map[string]string{"storage_max_file_bytes": "1073741824"}, http.StatusOK}, // 1 GiB max
		{map[string]string{"storage_max_file_bytes": "1048575"}, http.StatusBadRequest},
		{map[string]string{"storage_max_file_bytes": "1073741825"}, http.StatusBadRequest},
		{map[string]string{"storage_max_file_bytes": "nope"}, http.StatusBadRequest},
		{map[string]string{"storage_max_bytes": "0"}, http.StatusOK},             // unlimited
		{map[string]string{"storage_max_bytes": "1099511627776"}, http.StatusOK}, // 1 TiB
		{map[string]string{"storage_max_bytes": "-1"}, http.StatusBadRequest},
		{map[string]string{"storage_max_bytes": "1099511627777"}, http.StatusBadRequest},
		{map[string]string{"storage_retention_days": "0"}, http.StatusOK},
		{map[string]string{"storage_retention_days": "3650"}, http.StatusOK},
		{map[string]string{"storage_retention_days": "-1"}, http.StatusBadRequest},
		{map[string]string{"storage_retention_days": "3651"}, http.StatusBadRequest},
		{map[string]string{"storage_retention_days": "abc"}, http.StatusBadRequest},
		{map[string]string{"storage_path": strings.Repeat("x", 512)}, http.StatusOK},
		{map[string]string{"storage_path": strings.Repeat("x", 513)}, http.StatusBadRequest},
		{map[string]string{"bogus_key": "1"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		if got := set(c.body); got != c.want {
			t.Errorf("POST /api/settings %v: got %d want %d", c.body, got, c.want)
		}
	}

	// In-range values persist; clearing works.
	if got := set(map[string]string{"storage_retention_days": "7"}); got != http.StatusOK {
		t.Fatalf("set retention 7: got %d", got)
	}
	if v, err := e.st.GetSetting("storage_retention_days"); err != nil || v != "7" {
		t.Errorf("retention persisted = %q err %v, want \"7\"", v, err)
	}
	if got := set(map[string]string{"storage_retention_days": ""}); got != http.StatusOK {
		t.Fatalf("clear retention: got %d", got)
	}
	if v, err := e.st.GetSetting("storage_retention_days"); err == nil && v != "" {
		t.Errorf("retention after clear = %q, want empty", v)
	}
}

// TestStorageQuotaGuard: total usage + incoming > storage_max_bytes → 507
// "storage quota exceeded", and nothing is persisted.
func TestStorageQuotaGuard(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	_, w1tok := makeEmployee(t, e, "W1", store.KindAI, a)

	if err := e.st.SetSetting("storage_max_bytes", "100"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.InsertFile(0, "big.bin", "seed_big.bin", "application/octet-stream", 90); err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte("x"), 11) // 90 + 11 > 100
	code, body, _ := upload(t, e.srv.URL+"/api/files", w1tok, "file", "more.bin", payload)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("quota upload: got %d want 507: %v", code, body)
	}
	if body["error"] != "storage quota exceeded" {
		t.Errorf("quota error = %v, want 'storage quota exceeded'", body["error"])
	}
	u, err := e.st.StorageUsage()
	if err != nil {
		t.Fatal(err)
	}
	if u.FilesCount != 1 || u.FilesBytes != 90 {
		t.Errorf("usage after rejected upload = %+v, want 1 file / 90 bytes", u)
	}
}

// TestStorageDiskGuard: free disk space below the incoming size → 507
// "not enough free disk space" (the platform probe is stubbed so the test
// is deterministic).
func TestStorageDiskGuard(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	_, w1tok := makeEmployee(t, e, "W1", store.KindAI, a)

	prev := diskFreeBytes
	diskFreeBytes = func(string) (int64, error) { return 10, nil }
	t.Cleanup(func() { diskFreeBytes = prev })

	payload := bytes.Repeat([]byte("x"), 11) // 11 > 10 free bytes
	code, body, _ := upload(t, e.srv.URL+"/api/files", w1tok, "file", "d.bin", payload)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("disk guard: got %d want 507: %v", code, body)
	}
	if body["error"] != "not enough free disk space" {
		t.Errorf("disk error = %v, want 'not enough free disk space'", body["error"])
	}
}

// TestAdminStorageEndpoint: GET /api/admin/storage reports the effective
// policy and usage; admin-only, and disk_free_bytes degrades to -1 when the
// platform probe fails.
func TestAdminStorageEndpoint(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	_, w1tok := makeEmployee(t, e, "W1", store.KindAI, a)

	if _, err := e.st.InsertFile(0, "a.txt", "aa_a.txt", "application/octet-stream", 50); err != nil {
		t.Fatal(err)
	}

	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/admin/storage", w1tok, nil); code != http.StatusForbidden {
		t.Errorf("employee GET /api/admin/storage: got %d want 403", code)
	}
	if code, _ := req(t, http.MethodGet, e.srv.URL+"/api/admin/storage", "", nil); code != http.StatusUnauthorized {
		t.Errorf("no-token GET /api/admin/storage: got %d want 401", code)
	}

	code, body := req(t, http.MethodGet, e.srv.URL+"/api/admin/storage", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("admin GET /api/admin/storage: got %d: %v", code, body)
	}
	if body["quota_bytes"] != float64(defaultStorageQuota) {
		t.Errorf("quota_bytes = %v, want %d", body["quota_bytes"], defaultStorageQuota)
	}
	if body["max_file_bytes"] != float64(defaultMaxFileBytes) {
		t.Errorf("max_file_bytes = %v, want %d", body["max_file_bytes"], defaultMaxFileBytes)
	}
	if body["retention_days"] != float64(defaultRetentionDays) {
		t.Errorf("retention_days = %v, want %d", body["retention_days"], defaultRetentionDays)
	}
	if body["used_bytes"] != 50.0 {
		t.Errorf("used_bytes = %v, want 50", body["used_bytes"])
	}
	if p, _ := body["path"].(string); p != e.st.FilesDir {
		t.Errorf("path = %q, want %q", p, e.st.FilesDir)
	}
	files, _ := body["files"].(map[string]any)
	if files["count"] != 1.0 || files["bytes"] != 50.0 {
		t.Errorf("files = %v, want count 1 / 50 bytes", files)
	}
	arts, _ := body["artifacts"].(map[string]any)
	if arts["count"] != 0.0 {
		t.Errorf("artifacts = %v, want count 0", arts)
	}

	// Probe failure: endpoint stays 200, disk_free_bytes reports -1.
	prev := diskFreeBytes
	diskFreeBytes = func(string) (int64, error) { return 0, errors.New("no probe in tests") }
	t.Cleanup(func() { diskFreeBytes = prev })
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/admin/storage", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("admin GET (probe fail): got %d", code)
	}
	if body["disk_free_bytes"] != -1.0 {
		t.Errorf("disk_free_bytes = %v, want -1", body["disk_free_bytes"])
	}
}

// TestStorageDynamicCap: the per-file cap is settings-driven, not the old
// compile-time constant — raising it lets a formerly-rejected upload through
// and the 413 message names the effective cap.
func TestStorageDynamicCap(t *testing.T) {
	e := newEnv(t)
	a, _ := e.st.CreateDepartment("A", nil)
	_, w1tok := makeEmployee(t, e, "W1", store.KindAI, a)

	// Hermetic: stub the free-space probe so a machine with <11 MiB free
	// cannot flip the raised-cap upload from 201 to 507 (review finding #5).
	prev := diskFreeBytes
	diskFreeBytes = func(string) (int64, error) { return 1 << 30, nil }
	t.Cleanup(func() { diskFreeBytes = prev })

	big := bytes.Repeat([]byte{'A'}, (11<<20)+1)

	// Default cap (10 MiB): rejected, with the cap named in the message.
	code, body, _ := upload(t, e.srv.URL+"/api/files", w1tok, "file", "big.bin", big)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("default cap: got %d want 413: %v", code, body)
	}
	if body["error"] != "file exceeds 10 MiB limit" {
		t.Errorf("413 message = %v, want 'file exceeds 10 MiB limit'", body["error"])
	}

	// Raise the cap to 12 MiB: the same payload is now accepted.
	if err := e.st.SetSetting("storage_max_file_bytes", strconv.Itoa(12<<20)); err != nil {
		t.Fatal(err)
	}
	code, body, _ = upload(t, e.srv.URL+"/api/files", w1tok, "file", "big.bin", big)
	if code != http.StatusCreated {
		t.Fatalf("raised cap upload: got %d want 201: %v", code, body)
	}
	if body["size"] != float64(len(big)) {
		t.Errorf("size = %v, want %d", body["size"], len(big))
	}
}
