package store

import "fmt"

// Employee kinds.
const (
	KindAI    = "ai"
	KindHuman = "human"
)

// Employee is an AI or human team member.
type Employee struct {
	ID           int
	Name         string
	Role         string
	Kind         string
	DepartmentID int
	Tool         string
	ToolArgs     string
	Workdir      string
	TokenHash    string
	LastSeenAt   *string
	// Avatar is the stored_name of the employee's avatar file under
	// <FilesDir>/avatars/ ("" = use the built-in letter avatar).
	Avatar string
	// Email is the optional notification address (V-4); "" = no mailbox.
	Email string
}

// CountEmployees returns the total number of employees (P1b /metrics).
func (s *Store) CountEmployees() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM employees`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count employees: %w", err)
	}
	return n, nil
}

// EmployeeNames maps employee ids to their names in chunked batch queries
// (<=500 placeholders per statement, F-2 pattern) so callers avoid one
// query per row. Unknown ids are simply absent from the result map.
func (s *Store) EmployeeNames(ids []int) (map[int]string, error) {
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
		rows, err := s.db.Query("SELECT id, name FROM employees WHERE id IN ("+inPlaceholders(len(chunk))+")", args...)
		if err != nil {
			return nil, fmt.Errorf("store: employee names for %d ids: %w", len(ids), err)
		}
		for rows.Next() {
			var id int
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: employee names scan: %w", err)
			}
			out[id] = name
		}
		scanErr := rows.Err()
		rows.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("store: employee names rows: %w", scanErr)
		}
	}
	return out, nil
}
