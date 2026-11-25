package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrateV4 (V-4): adds employees.email and groups.webhook_url to a
// pre-V-4 database; no-op on a fresh database; idempotent on re-run.
func TestMigrateV4(t *testing.T) {
	s := openTestStore(t)
	if err := migrateV4(s.DB()); err != nil {
		t.Fatalf("re-run on fresh schema: %v", err)
	}

	legacy := filepath.Join(t.TempDir(), "legacy-v4.db")
	db, err := sql.Open("sqlite", legacy)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	legacySchema := `
		CREATE TABLE departments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			parent_id INTEGER,
			leader_id INTEGER,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE employees (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			department_id INTEGER,
			name TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'worker',
			kind TEXT NOT NULL DEFAULT 'ai',
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			created_by INTEGER NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			kind TEXT NOT NULL DEFAULT 'group',
			default_priority INTEGER NOT NULL DEFAULT 0,
			sla_timeout_seconds INTEGER NOT NULL DEFAULT 0
		);`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy tables: %v", err)
	}
	if err := migrateV4(db); err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	for _, col := range []struct{ table, name string }{
		{"employees", "email"},
		{"groups", "webhook_url"},
	} {
		var n int
		q := "SELECT COUNT(*) FROM pragma_table_info('" + col.table + "') WHERE name = '" + col.name + "'"
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("inspect %s.%s: %v", col.table, col.name, err)
		}
		if n != 1 {
			t.Errorf("%s.%s column count = %d, want 1", col.table, col.name, n)
		}
	}
	if err := migrateV4(db); err != nil {
		t.Fatalf("re-run on migrated legacy: %v", err)
	}
}

// TestEmployeeEmail (V-4): SetEmail round-trips through GetEmployee; an empty
// string clears the mailbox; an unknown employee is ErrNotFound.
func TestEmployeeEmail(t *testing.T) {
	s := openTestStore(t)
	_, _, _, worker := seedOrg(t, s)

	e, err := s.GetEmployee(worker)
	if err != nil {
		t.Fatalf("GetEmployee: %v", err)
	}
	if e.Email != "" {
		t.Fatalf("fresh employee email = %q, want empty", e.Email)
	}

	const addr = "bob@example.com"
	if err := s.SetEmail(worker, addr); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}
	e, err = s.GetEmployee(worker)
	if err != nil {
		t.Fatalf("GetEmployee after SetEmail: %v", err)
	}
	if e.Email != addr {
		t.Fatalf("email after set = %q, want %q", e.Email, addr)
	}

	if err := s.SetEmail(worker, ""); err != nil {
		t.Fatalf("SetEmail clear: %v", err)
	}
	e, err = s.GetEmployee(worker)
	if err != nil {
		t.Fatalf("GetEmployee after clear: %v", err)
	}
	if e.Email != "" {
		t.Fatalf("email after clear = %q, want empty", e.Email)
	}

	if err := s.SetEmail(99999, "x@y.example"); err == nil {
		t.Fatalf("SetEmail on missing employee: want error, got nil")
	} else if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetEmail on missing employee: err = %v, want ErrNotFound", err)
	}
}

// TestSetGroupWebhook (V-4): COALESCE semantics — a non-nil pointer sets the
// URL, nil clears it, an unknown group is ErrNotFound.
func TestSetGroupWebhook(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)
	gid, err := s.CreateGroup("v4-hook", leader, []int{worker}, "group")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	ctx := context.Background()

	if g, err := s.GetGroup(gid); err != nil || g.WebhookURL != "" {
		t.Fatalf("fresh group webhook = %q (err %v), want empty", g.WebhookURL, err)
	}

	url := "https://hooks.example.com/whatbark"
	if err := s.SetGroupWebhook(ctx, gid, &url); err != nil {
		t.Fatalf("SetGroupWebhook: %v", err)
	}
	g, err := s.GetGroup(gid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if g.WebhookURL != url {
		t.Fatalf("webhook after set = %q, want %q", g.WebhookURL, url)
	}

	if err := s.SetGroupWebhook(ctx, gid, nil); err != nil {
		t.Fatalf("SetGroupWebhook clear: %v", err)
	}
	if g, _ = s.GetGroup(gid); g.WebhookURL != "" {
		t.Fatalf("webhook after clear = %q, want empty", g.WebhookURL)
	}

	if err := s.SetGroupWebhook(ctx, 99999, &url); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetGroupWebhook unknown group: err = %v, want ErrNotFound", err)
	}
}

// TestEmployeeEmails (V-4): batch lookup returns only employees with a
// non-empty mailbox; unknown ids and mailbox-less employees are absent;
// an empty input yields an empty map without error.
func TestEmployeeEmails(t *testing.T) {
	s := openTestStore(t)
	_, _, leader, worker := seedOrg(t, s)
	const bob = "bob@example.com"
	const alice = "alice@example.com"
	if err := s.SetEmail(worker, bob); err != nil {
		t.Fatalf("SetEmail worker: %v", err)
	}
	if err := s.SetEmail(leader, alice); err != nil {
		t.Fatalf("SetEmail leader: %v", err)
	}

	m, err := s.EmployeeEmails([]int{worker, leader, 99999})
	if err != nil {
		t.Fatalf("EmployeeEmails: %v", err)
	}
	if len(m) != 2 || m[worker] != bob || m[leader] != alice {
		t.Fatalf("EmployeeEmails = %v, want exactly worker=%q leader=%q", m, bob, alice)
	}

	m, err = s.EmployeeEmails(nil)
	if err != nil {
		t.Fatalf("EmployeeEmails(nil): %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("EmployeeEmails(nil) = %v, want empty map", m)
	}
}
