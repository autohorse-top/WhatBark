package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Schedule is a recurring prompt that fires for one employee.
type Schedule struct {
	ID           int
	Name         string
	EmpID        int
	EverySeconds int
	Prompt       string
	Enabled      bool
	LastFiredAt  *string
}

// CreateSchedule inserts a schedule for an employee.
func (s *Store) CreateSchedule(name string, empID, everySeconds int, prompt string) (int, error) {
	if name == "" {
		return 0, fmt.Errorf("store: create schedule: name must not be empty")
	}
	if everySeconds <= 0 {
		return 0, fmt.Errorf("store: create schedule %q: every_seconds must be > 0", name)
	}
	if _, err := s.GetEmployee(empID); err != nil {
		return 0, fmt.Errorf("store: create schedule %q: employee: %w", name, err)
	}
	res, err := s.db.Exec(`
		INSERT INTO schedules (name, employee_id, every_seconds, prompt, enabled)
		VALUES (?, ?, ?, ?, 1)`, name, empID, everySeconds, prompt)
	if err != nil {
		return 0, fmt.Errorf("store: create schedule %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create schedule %q: %w", name, err)
	}
	return int(id), nil
}

// GetSchedule returns a schedule by id.
func (s *Store) GetSchedule(id int) (Schedule, error) {
	var sc Schedule
	var lastFired sql.NullString
	var enabled int
	err := s.db.QueryRow(`
		SELECT id, name, employee_id, every_seconds, prompt, enabled, last_fired_at
		FROM schedules WHERE id = ?`, id).
		Scan(&sc.ID, &sc.Name, &sc.EmpID, &sc.EverySeconds, &sc.Prompt, &enabled, &lastFired)
	if errors.Is(err, sql.ErrNoRows) {
		return sc, ErrNotFound
	}
	if err != nil {
		return sc, fmt.Errorf("store: get schedule %d: %w", id, err)
	}
	sc.Enabled = enabled != 0
	sc.LastFiredAt = nullString(lastFired)
	return sc, nil
}

// ListSchedules returns all schedules ordered by id.
func (s *Store) ListSchedules() ([]Schedule, error) {
	rows, err := s.db.Query(`
		SELECT id, name, employee_id, every_seconds, prompt, enabled, last_fired_at
		FROM schedules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list schedules: %w", err)
	}
	defer rows.Close()
	out := make([]Schedule, 0, 8)
	for rows.Next() {
		var sc Schedule
		var lastFired sql.NullString
		var enabled int
		if err := rows.Scan(&sc.ID, &sc.Name, &sc.EmpID, &sc.EverySeconds, &sc.Prompt, &enabled, &lastFired); err != nil {
			return nil, fmt.Errorf("store: list schedules: %w", err)
		}
		sc.Enabled = enabled != 0
		sc.LastFiredAt = nullString(lastFired)
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list schedules: %w", err)
	}
	return out, nil
}

// SetScheduleEnabled toggles a schedule on or off.
func (s *Store) SetScheduleEnabled(id int, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE schedules SET enabled = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("store: enable schedule %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FireSchedule stamps last_fired_at on a schedule.
func (s *Store) FireSchedule(id int, ts string) error {
	res, err := s.db.Exec(`UPDATE schedules SET last_fired_at = ? WHERE id = ?`, ts, id)
	if err != nil {
		return fmt.Errorf("store: fire schedule %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSchedule removes a schedule by id.
func (s *Store) DeleteSchedule(id int) error {
	res, err := s.db.Exec(`DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete schedule %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
