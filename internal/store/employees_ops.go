package store

import (
	"fmt"
)

// SetTool configures the execution tool of an employee (tool name + JSON args).
func (s *Store) SetTool(id int, tool, toolArgs string) error {
	if _, err := s.GetEmployee(id); err != nil {
		return fmt.Errorf("store: set tool: %w", err)
	}
	if tool == "" {
		return fmt.Errorf("store: set tool for employee %d: tool must not be empty", id)
	}
	if toolArgs == "" {
		toolArgs = "{}"
	}
	if _, err := s.db.Exec(`UPDATE employees SET tool = ?, tool_args = ? WHERE id = ?`, tool, toolArgs, id); err != nil {
		return fmt.Errorf("store: set tool for employee %d: %w", id, err)
	}
	return nil
}

// SetTokenHash stores the sha256 hex of the employee's bearer token.
func (s *Store) SetTokenHash(id int, hash string) error {
	if _, err := s.GetEmployee(id); err != nil {
		return fmt.Errorf("store: set token: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE employees SET token_hash = ? WHERE id = ?`, hash, id); err != nil {
		return fmt.Errorf("store: set token for employee %d: %w", id, err)
	}
	return nil
}

// SetAvatar stores the stored_name of the employee's avatar file
// ("" clears it). The on-disk file itself is managed by the API layer.
func (s *Store) SetAvatar(id int, avatar string) error {
	if _, err := s.GetEmployee(id); err != nil {
		return fmt.Errorf("store: set avatar: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE employees SET avatar = ? WHERE id = ?`, avatar, id); err != nil {
		return fmt.Errorf("store: set avatar for employee %d: %w", id, err)
	}
	return nil
}

// SetEmail stores an optional notification mailbox (V-4). An empty string
// clears the mailbox (stored as NULL).
func (s *Store) SetEmail(id int, email string) error {
	if _, err := s.GetEmployee(id); err != nil {
		return fmt.Errorf("store: set email: %w", err)
	}
	var val any
	if email != "" {
		val = email
	}
	if _, err := s.db.Exec(`UPDATE employees SET email = ? WHERE id = ?`, val, id); err != nil {
		return fmt.Errorf("store: set email for employee %d: %w", id, err)
	}
	return nil
}

// UpdateLastSeen records an agent heartbeat timestamp (RFC3339 UTC).
func (s *Store) UpdateLastSeen(id int, ts string) error {
	res, err := s.db.Exec(`UPDATE employees SET last_seen_at = ? WHERE id = ?`, ts, id)
	if err != nil {
		return fmt.Errorf("store: heartbeat for employee %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
