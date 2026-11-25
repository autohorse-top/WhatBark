package store

import (
	"errors"
	"testing"
)

// V-2: admin_tokens — create/list/get/delete/lookup/ensure.
func TestAdminTokens(t *testing.T) {
	s := openTestStore(t)
	var opsID int
	opsPlain := ""

	t.Run("create and lookup", func(t *testing.T) {
		row, plain, err := s.CreateAdminToken("ops")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		opsID, opsPlain = row.ID, plain
		if row.Name != "ops" || row.ID <= 0 || row.CreatedAt == "" {
			t.Fatalf("row = %+v", row)
		}
		if len(plain) != 32 {
			t.Fatalf("plaintext token length = %d, want 32", len(plain))
		}
		id, err := s.LookupAdminToken(plain)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if id != row.ID {
			t.Fatalf("lookup id = %d, want %d", id, row.ID)
		}
	})

	t.Run("lookup unknown token", func(t *testing.T) {
		if _, err := s.LookupAdminToken("nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("duplicate name rejected", func(t *testing.T) {
		if _, _, err := s.CreateAdminToken("ops"); err == nil {
			t.Fatal("expected error for duplicate name")
		}
	})

	t.Run("invalid names rejected", func(t *testing.T) {
		for _, name := range []string{"", " ", "bad/name", "a b", string(make([]byte, 65))} {
			if _, _, err := s.CreateAdminToken(name); err == nil {
				t.Fatalf("name %q: expected error", name)
			}
		}
	})

	t.Run("list ordered by id, no hash exposed", func(t *testing.T) {
		if _, _, err := s.CreateAdminToken("zeta"); err != nil {
			t.Fatalf("create zeta: %v", err)
		}
		rows, err := s.ListAdminTokens()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("len = %d, want 2", len(rows))
		}
		if rows[0].Name != "ops" || rows[1].Name != "zeta" {
			t.Fatalf("order = %v", []string{rows[0].Name, rows[1].Name})
		}
		var n int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM admin_tokens WHERE token_hash IS NULL OR token_hash = ''`).Scan(&n); err != nil {
			t.Fatalf("hash check: %v", err)
		}
		if n != 0 {
			t.Fatalf("%d rows have empty token_hash", n)
		}
	})

	t.Run("delete invalidates token", func(t *testing.T) {
		if err := s.DeleteAdminToken(opsID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		// the deleted token stops resolving immediately
		if _, err := s.LookupAdminToken(opsPlain); !errors.Is(err, ErrNotFound) {
			t.Fatalf("lookup deleted token: err = %v, want ErrNotFound", err)
		}
		// the name is freed: a new token with the same name can be created
		if _, _, err := s.CreateAdminToken("ops"); err != nil {
			t.Fatalf("recreate ops: %v", err)
		}
		// deleting an unknown id is ErrNotFound
		if err := s.DeleteAdminToken(opsID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete missing: err = %v, want ErrNotFound", err)
		}
	})
}

func TestEnsureAdminToken(t *testing.T) {
	s := openTestStore(t)

	id1, err := s.EnsureAdminToken("boss", "boot-token-1")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// idempotent: same token again returns the same id
	id2, err := s.EnsureAdminToken("boss", "boot-token-1")
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("ensure not idempotent: %d vs %d", id1, id2)
	}
	if got, err := s.LookupAdminToken("boot-token-1"); err != nil || got != id1 {
		t.Fatalf("lookup after ensure = %d, %v", got, err)
	}

	// same name taken by a DIFFERENT token -> fallback name
	id3, err := s.EnsureAdminToken("boss", "boot-token-2")
	if err != nil {
		t.Fatalf("ensure with colliding name: %v", err)
	}
	row, err := s.GetAdminToken(id3)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Name != "boss-legacy" {
		t.Fatalf("fallback name = %q, want boss-legacy", row.Name)
	}

	// empty token rejected
	if _, err := s.EnsureAdminToken("boss", ""); err == nil {
		t.Fatal("expected error for empty token")
	}
}
