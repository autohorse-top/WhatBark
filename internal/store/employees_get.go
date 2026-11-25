package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// GetEmployee returns an employee by id.
func (s *Store) GetEmployee(id int) (Employee, error) {
	var e Employee
	var lastSeen, email sql.NullString
	err := s.db.QueryRow(`
		SELECT id, name, role, kind, department_id, tool, tool_args, workdir, token_hash, last_seen_at, avatar, email
		FROM employees WHERE id = ?`, id).
		Scan(&e.ID, &e.Name, &e.Role, &e.Kind, &e.DepartmentID, &e.Tool, &e.ToolArgs, &e.Workdir, &e.TokenHash, &lastSeen, &e.Avatar, &email)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, fmt.Errorf("store: get employee %d: %w", id, err)
	}
	e.LastSeenAt = nullString(lastSeen)
	e.Email = email.String
	return e, nil
}

// EmployeeEmails maps employee ids to their mailbox addresses (V-4) in
// chunked batch queries (<=500 placeholders per statement, F-2 pattern).
// Unknown ids and empty (NULL) mailboxes are simply absent from the result.
func (s *Store) EmployeeEmails(ids []int) (map[int]string, error) {
	out := make(map[int]string, len(ids))
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.Query(`SELECT id, email FROM employees WHERE email IS NOT NULL AND email != '' AND id IN (`+inPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("store: employee emails for %d ids: %w", len(chunk), err)
		}
		for rows.Next() {
			var id int
			var mail string
			if err := rows.Scan(&id, &mail); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: employee emails scan: %w", err)
			}
			out[id] = mail
		}
		scanErr := rows.Err()
		rows.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("store: employee emails rows: %w", scanErr)
		}
	}
	return out, nil
}
