package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// V-2: admin_tokens — multiple named admin tokens. Only the sha256 digest is
// persisted; the plaintext exists only in the creation response. The boot
// token (serve --admin-token or the settings-generated one) is seeded into
// this table at startup by the API layer, so ALL admin auth is a single
// table lookup.

// AdminToken is one row of admin_tokens (no hash, no plaintext exposed).
type AdminToken struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// adminTokenNameRe matches employee names' shape: safe UI fragment.
var adminTokenNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NewAdminTokenPlain returns a fresh 32-char hex admin token.
func NewAdminTokenPlain() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: generate admin token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// hashAdminToken is the stored digest form of a plaintext admin token.
func hashAdminToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// CreateAdminToken inserts a new named admin token and returns the row plus
// the plaintext token (shown once, never stored). Invalid or duplicate names
// return descriptive errors (400 by the API layer).
func (s *Store) CreateAdminToken(name string) (AdminToken, string, error) {
	name = strings.TrimSpace(name)
	if !adminTokenNameRe.MatchString(name) {
		return AdminToken{}, "", fmt.Errorf("admin token name must match [A-Za-z0-9_-]{1,64}, got %q", name)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_tokens WHERE name = ?`, name).Scan(&n); err != nil {
		return AdminToken{}, "", fmt.Errorf("store: check admin token name: %w", err)
	}
	if n > 0 {
		return AdminToken{}, "", fmt.Errorf("admin token name %q already exists", name)
	}
	plain, err := NewAdminTokenPlain()
	if err != nil {
		return AdminToken{}, "", err
	}
	res, err := s.db.Exec(`INSERT INTO admin_tokens (name, token_hash) VALUES (?, ?)`, name, hashAdminToken(plain))
	if err != nil {
		return AdminToken{}, "", fmt.Errorf("store: create admin token %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AdminToken{}, "", fmt.Errorf("store: create admin token %q: %w", name, err)
	}
	row, err := s.GetAdminToken(int(id))
	if err != nil {
		return AdminToken{}, "", err
	}
	return row, plain, nil
}

// EnsureAdminToken seeds a boot token as name if its hash is not registered
// yet (idempotent: restarts with the same boot token are no-ops). If the name
// is already taken by a different token, falls back to name+"-legacy".
// Returns the row id.
func (s *Store) EnsureAdminToken(name, plain string) (int, error) {
	if plain == "" {
		return 0, fmt.Errorf("store: ensure admin token: empty token")
	}
	hash := hashAdminToken(plain)
	var id int
	err := s.db.QueryRow(`SELECT id FROM admin_tokens WHERE token_hash = ?`, hash).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: lookup admin token: %w", err)
	}
	for _, n := range []string{name, name + "-legacy"} {
		var used int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_tokens WHERE name = ?`, n).Scan(&used); err != nil {
			return 0, fmt.Errorf("store: ensure admin token: %w", err)
		}
		if used > 0 {
			continue
		}
		res, ierr := s.db.Exec(`INSERT INTO admin_tokens (name, token_hash) VALUES (?, ?)`, n, hash)
		if ierr != nil {
			return 0, fmt.Errorf("store: ensure admin token %q: %w", n, ierr)
		}
		ins, lerr := res.LastInsertId()
		if lerr != nil {
			return 0, fmt.Errorf("store: ensure admin token: %w", lerr)
		}
		return int(ins), nil
	}
	return 0, fmt.Errorf("store: ensure admin token: name %q already taken", name)
}

// LookupAdminToken returns the row id for a plaintext token (sha256 match)
// or ErrNotFound.
func (s *Store) LookupAdminToken(plain string) (int, error) {
	var id int
	err := s.db.QueryRow(`SELECT id FROM admin_tokens WHERE token_hash = ?`, hashAdminToken(plain)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: lookup admin token: %w", err)
	}
	return id, nil
}

// GetAdminToken returns a single row by id (no hash exposed); ErrNotFound
// if absent.
func (s *Store) GetAdminToken(id int) (AdminToken, error) {
	var t AdminToken
	err := s.db.QueryRow(
		`SELECT id, name, created_at FROM admin_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminToken{}, fmt.Errorf("%w: admin token %d", ErrNotFound, id)
	}
	if err != nil {
		return AdminToken{}, fmt.Errorf("store: get admin token %d: %w", id, err)
	}
	return t, nil
}

// ListAdminTokens lists all rows ordered by id (no hashes exposed).
func (s *Store) ListAdminTokens() ([]AdminToken, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM admin_tokens ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list admin tokens: %w", err)
	}
	defer rows.Close()
	out := []AdminToken{}
	for rows.Next() {
		var t AdminToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list admin tokens: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list admin tokens: %w", err)
	}
	return out, nil
}

// DeleteAdminToken removes a row; ErrNotFound if absent. Auth is a live
// table lookup, so the token stops working immediately.
func (s *Store) DeleteAdminToken(id int) error {
	res, err := s.db.Exec(`DELETE FROM admin_tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete admin token %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete admin token %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: admin token %d", ErrNotFound, id)
	}
	return nil
}
