package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Project groups work; optionally bound to a workflow and an entry employee.
type Project struct {
	ID         int
	Name       string
	Brief      string
	WorkflowID *int
	EntryEmpID *int
	CreatedAt  string
}

// CreateProject inserts a project.
func (s *Store) CreateProject(name, brief string, workflowID, entryEmpID *int) (int, error) {
	if name == "" {
		return 0, fmt.Errorf("store: create project: name must not be empty")
	}
	if workflowID != nil {
		if _, err := s.GetWorkflow(*workflowID); err != nil {
			return 0, fmt.Errorf("store: create project %q: workflow: %w", name, err)
		}
	}
	if entryEmpID != nil {
		if _, err := s.GetEmployee(*entryEmpID); err != nil {
			return 0, fmt.Errorf("store: create project %q: entry employee: %w", name, err)
		}
	}
	res, err := s.db.Exec(`INSERT INTO projects (name, brief, workflow_id, entry_employee_id) VALUES (?, ?, ?, ?)`,
		name, brief, workflowID, entryEmpID)
	if err != nil {
		return 0, fmt.Errorf("store: create project %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create project %q: %w", name, err)
	}
	return int(id), nil
}

// GetProject returns a project by id.
func (s *Store) GetProject(id int) (Project, error) {
	var p Project
	var wf, entry sql.NullInt64
	var createdAt string
	err := s.db.QueryRow(`
		SELECT id, name, brief, workflow_id, entry_employee_id, created_at
		FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Brief, &wf, &entry, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, fmt.Errorf("store: get project %d: %w", id, err)
	}
	p.WorkflowID = nullInt64(wf)
	p.EntryEmpID = nullInt64(entry)
	p.CreatedAt = createdAt
	return p, nil
}

// ListProjects returns all projects ordered by id.
func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`
		SELECT id, name, brief, workflow_id, entry_employee_id, created_at
		FROM projects ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	defer rows.Close()
	out := make([]Project, 0, 8)
	for rows.Next() {
		var p Project
		var wf, entry sql.NullInt64
		var createdAt string
		if err := rows.Scan(&p.ID, &p.Name, &p.Brief, &wf, &entry, &createdAt); err != nil {
			return nil, fmt.Errorf("store: list projects: %w", err)
		}
		p.WorkflowID = nullInt64(wf)
		p.EntryEmpID = nullInt64(entry)
		p.CreatedAt = createdAt
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	return out, nil
}

// UpdateProject replaces the brief/workflow/entry of a project.
func (s *Store) UpdateProject(id int, brief string, workflowID, entryEmpID *int) error {
	if _, err := s.GetProject(id); err != nil {
		return fmt.Errorf("store: update project %d: %w", id, err)
	}
	if workflowID != nil {
		if _, err := s.GetWorkflow(*workflowID); err != nil {
			return fmt.Errorf("store: update project %d: workflow: %w", id, err)
		}
	}
	if entryEmpID != nil {
		if _, err := s.GetEmployee(*entryEmpID); err != nil {
			return fmt.Errorf("store: update project %d: entry employee: %w", id, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE projects SET brief = ?, workflow_id = ?, entry_employee_id = ? WHERE id = ?`,
		brief, workflowID, entryEmpID, id); err != nil {
		return fmt.Errorf("store: update project %d: %w", id, err)
	}
	return nil
}

// DeleteProject removes a project that has no tasks. Otherwise ErrInUse.
func (s *Store) DeleteProject(id int) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE project_id = ?`, id).Scan(&n); err != nil {
		return fmt.Errorf("store: delete project %d: %w", id, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: project %d has tasks", ErrInUse, id)
	}
	res, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete project %d: %w", id, err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

// ProjectNames maps project ids to their names in chunked batch queries
// (<=500 placeholders per statement, F-2 pattern) so callers avoid one
// query per row. Unknown ids are simply absent from the result map.
func (s *Store) ProjectNames(ids []int) (map[int]string, error) {
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
		rows, err := s.db.Query("SELECT id, name FROM projects WHERE id IN ("+inPlaceholders(len(chunk))+")", args...)
		if err != nil {
			return nil, fmt.Errorf("store: project names for %d ids: %w", len(ids), err)
		}
		for rows.Next() {
			var id int
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: project names scan: %w", err)
			}
			out[id] = name
		}
		scanErr := rows.Err()
		rows.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("store: project names rows: %w", scanErr)
		}
	}
	return out, nil
}
