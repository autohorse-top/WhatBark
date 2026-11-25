package store

import (
	"fmt"
)

// DeleteEmployee removes an employee that is not a department leader and has
// no tasks. Otherwise ErrInUse is returned.
func (s *Store) DeleteEmployee(id int) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM departments WHERE leader_id = ?`, id).Scan(&n); err != nil {
		return fmt.Errorf("store: delete employee %d: %w", id, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: employee %d is a department leader", ErrInUse, id)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE employee_id = ?`, id).Scan(&n); err != nil {
		return fmt.Errorf("store: delete employee %d: %w", id, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: employee %d has tasks", ErrInUse, id)
	}
	// S14: drop group memberships first (no FK on group_members.employee_id,
	// boss=0 rows are never deleted so this is a plain member-row cleanup).
	if _, err := s.db.Exec(`DELETE FROM group_members WHERE employee_id = ?`, id); err != nil {
		return fmt.Errorf("store: delete employee %d: %w", id, err)
	}
	res, err := s.db.Exec(`DELETE FROM employees WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete employee %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
