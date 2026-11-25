package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// nameRe restricts employee names to safe path fragments (S7-Minor8).
// Names are used to build the default workdir "work/<name>", so path
// traversal characters and whitespace are prohibited.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrNameTaken signals that an employee with the same name already exists
// (employees.name UNIQUE). Callers use errors.Is to map it to HTTP 409.
var ErrNameTaken = errors.New("store: employee name already taken")

// CreateEmployee inserts an AI or human employee into a department.
func (s *Store) CreateEmployee(name, role, kind string, departmentID int) (int, error) {
	if !nameRe.MatchString(name) {
		return 0, fmt.Errorf("store: employee name must match [A-Za-z0-9_-]{1,64}, got %q", name)
	}
	if role == "" {
		return 0, fmt.Errorf("store: employee %s: role must not be empty", name)
	}
	if kind != KindAI && kind != KindHuman {
		return 0, fmt.Errorf("store: employee %s: unsupported kind", name)
	}
	if _, err := s.GetDepartment(departmentID); err != nil {
		return 0, fmt.Errorf("store: employee %s: %w", name, err)
	}
	// Fast path for duplicates; the UNIQUE index remains the backstop for
	// concurrent inserts (handled below).
	if exists, err := s.EmployeeExists(name); err != nil {
		return 0, fmt.Errorf("store: employee %s: %w", name, err)
	} else if exists {
		return 0, fmt.Errorf("store: employee %s: %w", name, ErrNameTaken)
	}
	workdir := "work/" + name
	res, err := s.db.Exec(`
		INSERT INTO employees (name, role, kind, department_id, tool, tool_args, workdir)
		VALUES (?, ?, ?, ?, 'mock', '{}', ?)`, name, role, kind, departmentID, workdir)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("store: employee %s: %w", name, ErrNameTaken)
		}
		return 0, fmt.Errorf("store: employee %s: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: employee %s: %w", name, err)
	}
	return int(id), nil
}

// EmployeeExists reports whether an employee with the given name exists.
func (s *Store) EmployeeExists(name string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM employees WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: find employee %s: %w", name, err)
	}
	return true, nil
}
