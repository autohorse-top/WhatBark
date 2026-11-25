package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Department is a node in the multi-level org tree.
type Department struct {
	ID       int
	Name     string
	ParentID *int // nil for top-level departments
	LeaderID *int // employee id, nil when no leader is set
	GroupID  *int // S31: the department's own channel (groups.kind='channel'); nil for legacy departments
}

// maxDepartmentNameRunes mirrors the group-name cap (S31: department and group
// names share the same naming rules).
const maxDepartmentNameRunes = 64

// validateDepartmentName trims whitespace and enforces the S31 name rule:
// 1..64 runes. Returns the trimmed name.
func validateDepartmentName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxDepartmentNameRunes {
		return "", fmt.Errorf("store: department name must be 1-%d characters", maxDepartmentNameRunes)
	}
	return name, nil
}

// CreateDepartment inserts a department. parentID nil means top level.
// Name is trimmed and must be 1..64 runes (S31); unique per parent level
// (enforced at the app level; SQLite's UNIQUE constraint treats NULL
// parents as distinct, so the table-level constraint alone cannot catch
// top-level duplicates). Legacy path: no channel is created, group_id stays
// NULL.
func (s *Store) CreateDepartment(name string, parentID *int) (int, error) {
	name, err := validateDepartmentName(name)
	if err != nil {
		return 0, err
	}
	if parentID != nil {
		if _, err := s.GetDepartment(*parentID); err != nil {
			return 0, fmt.Errorf("store: create department %q: parent: %w", name, err)
		}
	}
	var dup int
	if parentID == nil {
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM departments WHERE name = ? AND parent_id IS NULL`, name).Scan(&dup); err != nil {
			return 0, fmt.Errorf("store: create department %q: %w", name, err)
		}
	} else {
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM departments WHERE name = ? AND parent_id = ?`, name, *parentID).Scan(&dup); err != nil {
			return 0, fmt.Errorf("store: create department %q: %w", name, err)
		}
	}
	if dup > 0 {
		return 0, fmt.Errorf("store: create department %q: name already exists at this level", name)
	}
	res, err := s.db.Exec(`INSERT INTO departments (name, parent_id) VALUES (?, ?)`, name, parentID)
	if err != nil {
		return 0, fmt.Errorf("store: create department %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create department %q: %w", name, err)
	}
	return int(id), nil
}

// GetDepartment returns a department by id.
func (s *Store) GetDepartment(id int) (Department, error) {
	var d Department
	var parent, leader, group sql.NullInt64
	err := s.db.QueryRow(`SELECT id, name, parent_id, leader_id, group_id FROM departments WHERE id = ?`, id).
		Scan(&d.ID, &d.Name, &parent, &leader, &group)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, fmt.Errorf("store: get department %d: %w", id, err)
	}
	d.ParentID = nullInt64(parent)
	d.LeaderID = nullInt64(leader)
	d.GroupID = nullInt64(group)
	return d, nil
}

// ListDepartments returns all departments ordered by id.
func (s *Store) ListDepartments() ([]Department, error) {
	rows, err := s.db.Query(`SELECT id, name, parent_id, leader_id, group_id FROM departments ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list departments: %w", err)
	}
	defer rows.Close()
	out := make([]Department, 0, 8)
	for rows.Next() {
		var d Department
		var parent, leader, group sql.NullInt64
		if err := rows.Scan(&d.ID, &d.Name, &parent, &leader, &group); err != nil {
			return nil, fmt.Errorf("store: list departments: %w", err)
		}
		d.ParentID = nullInt64(parent)
		d.LeaderID = nullInt64(leader)
		d.GroupID = nullInt64(group)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list departments: %w", err)
	}
	return out, nil
}

// CreateDepartmentWithChannel (S31) creates a department together with its
// channel in one transaction: validate (trimmed 1..64 runes, parent exists,
// unique per level) -> insert department -> insert channel group
// (kind=channel, created_by=0, default policy 0/0, name = department name)
// -> boss(0) joins group_members -> system message
// "boss created a department channel "<name>"" (from_id NULL, same
// convention as CreateGroup's creator-0 system message) -> write
// departments.group_id back. Any failure rolls everything back.
func (s *Store) CreateDepartmentWithChannel(name string, parentID *int) (int, int, error) {
	name, err := validateDepartmentName(name)
	if err != nil {
		return 0, 0, err
	}
	if parentID != nil {
		if _, err := s.GetDepartment(*parentID); err != nil {
			return 0, 0, fmt.Errorf("store: create department %q: parent: %w", name, err)
		}
	}
	var dup int
	if parentID == nil {
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM departments WHERE name = ? AND parent_id IS NULL`, name).Scan(&dup); err != nil {
			return 0, 0, fmt.Errorf("store: create department %q: %w", name, err)
		}
	} else {
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM departments WHERE name = ? AND parent_id = ?`, name, *parentID).Scan(&dup); err != nil {
			return 0, 0, fmt.Errorf("store: create department %q: %w", name, err)
		}
	}
	if dup > 0 {
		return 0, 0, fmt.Errorf("store: create department %q: name already exists at this level", name)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("store: create department %q with channel: %w", name, err)
	}
	res, err := tx.Exec(`INSERT INTO departments (name, parent_id) VALUES (?, ?)`, name, parentID)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("store: create department %q with channel: %w", name, err)
	}
	deptID, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("store: create department %q with channel: %w", name, err)
	}
	res, err = tx.Exec(`INSERT INTO groups (name, created_by, kind) VALUES (?, 0, 'channel')`, name)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("store: create department %q with channel: %w", name, err)
	}
	gid, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("store: create department %q with channel: %w", name, err)
	}
	if _, err := tx.Exec(`INSERT INTO group_members (group_id, employee_id) VALUES (?, 0)`, gid); err != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("store: create department %q with channel: boss member: %w", name, err)
	}
	body := fmt.Sprintf("boss created a department channel \"%s\"", name)
	if _, err := tx.Exec(`
		INSERT INTO messages (from_id, to_id, kind, body, group_id)
		VALUES (NULL, NULL, ?, ?, ?)`, MsgSystem, body, gid); err != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("store: create department %q with channel: insert system message: %w", name, err)
	}
	if _, err := tx.Exec(`UPDATE departments SET group_id = ? WHERE id = ?`, gid, deptID); err != nil {
		_ = tx.Rollback()
		return 0, 0, fmt.Errorf("store: create department %q with channel: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("store: create department %q with channel: %w", name, err)
	}
	return int(deptID), int(gid), nil
}

// RenameDepartment (S31) renames a department. The name is trimmed and must
// be 1..64 runes; unique per parent level excluding the department itself.
// When the department owns a channel, the group row is renamed in the same
// transaction (H-3/S31 N1): a concurrently deleted channel aborts the whole
// rename (department name left untouched) and the error is a wrapped
// ErrNotFound. Returns the updated department (including GroupID). An
// unknown department is ErrNotFound.
func (s *Store) RenameDepartment(id int, name string) (Department, error) {
	name, err := validateDepartmentName(name)
	if err != nil {
		return Department{}, err
	}
	cur, err := s.GetDepartment(id)
	if err != nil {
		return Department{}, err
	}
	// Single-connection pool: every statement below goes through tx.
	tx, err := s.db.Begin()
	if err != nil {
		return Department{}, fmt.Errorf("store: rename department %d: %w", id, err)
	}
	var dup int
	if cur.ParentID == nil {
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM departments WHERE name = ? AND parent_id IS NULL AND id <> ?`, name, id).Scan(&dup); err != nil {
			_ = tx.Rollback()
			return Department{}, fmt.Errorf("store: rename department %d: %w", id, err)
		}
	} else {
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM departments WHERE name = ? AND parent_id = ? AND id <> ?`, name, *cur.ParentID, id).Scan(&dup); err != nil {
			_ = tx.Rollback()
			return Department{}, fmt.Errorf("store: rename department %d: %w", id, err)
		}
	}
	if dup > 0 {
		_ = tx.Rollback()
		return Department{}, fmt.Errorf("store: rename department %d: name %q already exists at this level", id, name)
	}
	if _, err := tx.Exec(`UPDATE departments SET name = ? WHERE id = ?`, name, id); err != nil {
		_ = tx.Rollback()
		return Department{}, fmt.Errorf("store: rename department %d: %w", id, err)
	}
	if cur.GroupID != nil {
		// The channel name must follow the department name; both updates
		// commit together. The name already passed validateDepartmentName
		// (1-64 runes), the same rule RenameGroup enforces.
		res, err := tx.Exec(`UPDATE groups SET name = ? WHERE id = ?`, name, *cur.GroupID)
		if err != nil {
			_ = tx.Rollback()
			return Department{}, fmt.Errorf("store: rename department %d channel: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			_ = tx.Rollback()
			return Department{}, fmt.Errorf("store: rename department %d channel: %w", id, err)
		} else if n == 0 {
			// The channel row was deleted concurrently: roll back the
			// department rename too and surface ErrNotFound.
			_ = tx.Rollback()
			return Department{}, fmt.Errorf("store: rename department %d: %w", id, ErrNotFound)
		}
	}
	if err := tx.Commit(); err != nil {
		return Department{}, fmt.Errorf("store: rename department %d: %w", id, err)
	}
	return s.GetDepartment(id)
}

// SetDepartmentLeader makes empID the (single) leader of deptID.
// The employee must belong to that department.
func (s *Store) SetDepartmentLeader(deptID, empID int) error {
	if _, err := s.GetDepartment(deptID); err != nil {
		return fmt.Errorf("store: set leader: department: %w", err)
	}
	emp, err := s.GetEmployee(empID)
	if err != nil {
		return fmt.Errorf("store: set leader: employee: %w", err)
	}
	if emp.DepartmentID != deptID {
		return fmt.Errorf("store: set leader: employee %d belongs to department %d, not %d", empID, emp.DepartmentID, deptID)
	}
	if _, err := s.db.Exec(`UPDATE departments SET leader_id = ? WHERE id = ?`, empID, deptID); err != nil {
		return fmt.Errorf("store: set leader for department %d: %w", deptID, err)
	}
	return nil
}

// DeleteDepartment removes a department that has no child departments and
// no employees. Otherwise ErrInUse is returned.
func (s *Store) DeleteDepartment(id int) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM departments WHERE parent_id = ?`, id).Scan(&n); err != nil {
		return fmt.Errorf("store: delete department %d: %w", id, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: department %d has child departments", ErrInUse, id)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM employees WHERE department_id = ?`, id).Scan(&n); err != nil {
		return fmt.Errorf("store: delete department %d: %w", id, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: department %d has employees", ErrInUse, id)
	}
	res, err := s.db.Exec(`DELETE FROM departments WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete department %d: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete department %d: %w", id, err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDepartmentAndChannel (S31) deletes a department and its channel in
// one transaction when a channel exists. Pre-checks (no child departments,
// no employees) return ErrInUse; unknown department is ErrNotFound. When the
// department owns a channel, the open-task count is re-checked inside the
// transaction (TOCTOU guard, same policy as DeleteGroup) and aborts with
// ErrGroupHasOpenTasks. Deleting the group row relies on FK actions:
// departments.group_id -> SET NULL, group_members/group_prefs/messages ->
// CASCADE, tasks.group_id -> SET NULL (task history preserved). The channel's
// conv_reads cursors (peer_id = -(gid+1), no FK) are cleared explicitly.
// Returns the deleted channel id, or nil for legacy departments without a
// channel (deleted with the plain row delete).
func (s *Store) DeleteDepartmentAndChannel(id int) (*int, error) {
	d, err := s.GetDepartment(id)
	if err != nil {
		return nil, err
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM departments WHERE parent_id = ?`, id).Scan(&n); err != nil {
		return nil, fmt.Errorf("store: delete department %d: %w", id, err)
	}
	if n > 0 {
		return nil, fmt.Errorf("%w: department %d has child departments", ErrInUse, id)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM employees WHERE department_id = ?`, id).Scan(&n); err != nil {
		return nil, fmt.Errorf("store: delete department %d: %w", id, err)
	}
	if n > 0 {
		return nil, fmt.Errorf("%w: department %d has employees", ErrInUse, id)
	}
	if d.GroupID == nil {
		// Legacy department without a channel: plain row delete.
		res, err := s.db.Exec(`DELETE FROM departments WHERE id = ?`, id)
		if err != nil {
			return nil, fmt.Errorf("store: delete department %d: %w", id, err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("store: delete department %d: %w", id, err)
		}
		if rows == 0 {
			return nil, ErrNotFound
		}
		return nil, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: delete department %d: %w", id, err)
	}
	var open int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE group_id = ? AND status IN ('queued', 'running')`, *d.GroupID).Scan(&open); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("store: delete department %d: %w", id, err)
	}
	if open > 0 {
		_ = tx.Rollback()
		return nil, ErrGroupHasOpenTasks{Count: open}
	}
	if _, err := tx.Exec(`DELETE FROM conv_reads WHERE peer_id = ?`, groupPeerID(*d.GroupID)); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("store: delete department %d: clear read cursors: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM groups WHERE id = ?`, *d.GroupID); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("store: delete department %d: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM departments WHERE id = ?`, id); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("store: delete department %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: delete department %d: %w", id, err)
	}
	return d.GroupID, nil
}
