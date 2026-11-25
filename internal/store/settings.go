package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SetSetting stores a key/value setting (insert or replace).
func (s *Store) SetSetting(key, value string) error {
	if key == "" {
		return fmt.Errorf("store: set setting: key must not be empty")
	}
	if _, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("store: set setting %s: %w", key, err)
	}
	return nil
}

// GetSetting returns the value for a key; ErrNotFound if absent.
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: get setting %s: %w", key, err)
	}
	return v, nil
}

// GetSettingDefault returns the value for a key or def when absent.
func (s *Store) GetSettingDefault(key, def string) string {
	v, err := s.GetSetting(key)
	if err != nil {
		return def
	}
	return v
}

// ListSettings returns all settings ordered by key.
func (s *Store) ListSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, 8)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: list settings: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	return out, nil
}
