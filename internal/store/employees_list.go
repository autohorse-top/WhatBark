package store

import (
	"database/sql"
	"fmt"
)

// ListEmployees returns all employees ordered by id.
func (s *Store) ListEmployees() ([]Employee, error) {
	rows, err := s.db.Query(`
		SELECT id, name, role, kind, department_id, tool, tool_args, workdir, token_hash, last_seen_at, avatar, email
		FROM employees ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list employees: %w", err)
	}
	defer rows.Close()
	out := make([]Employee, 0, 16)
	for rows.Next() {
		var e Employee
		var lastSeen, email sql.NullString
		if err := rows.Scan(&e.ID, &e.Name, &e.Role, &e.Kind, &e.DepartmentID, &e.Tool, &e.ToolArgs, &e.Workdir, &e.TokenHash, &lastSeen, &e.Avatar, &email); err != nil {
			return nil, fmt.Errorf("store: list employees: %w", err)
		}
		e.LastSeenAt = nullString(lastSeen)
		e.Email = email.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list employees: %w", err)
	}
	return out, nil
}
