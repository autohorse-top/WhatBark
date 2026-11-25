package store

import (
	"fmt"
)

// H-7: append-only audit log. The API layer records admin mutations (via the
// adminOnly hook), task cancel/reassign/preempt, and auth failures. Rows are
// never updated or deleted by the application.

// AuditEntry is one row of the audit_log table.
type AuditEntry struct {
	ID        int    `json:"id"`
	TS        string `json:"ts"`         // UTC "2006-01-02 15:04:05" (datetime('now'))
	ActorKind string `json:"actor_kind"` // "admin" | "employee" | "anon"
	ActorID   int    `json:"actor_id"`   // employee id; 0 for admin/anon
	Action    string `json:"action"`     // e.g. "employee.create", "task.cancel", "auth.fail"
	Target    string `json:"target"`     // request path or resource path, e.g. "/api/tasks/3"
	Detail    string `json:"detail"`     // free-form context (reasons, targets)
}

// AuditAppend records one entry. Callers may ignore the error: audit logging
// must never break the primary operation, but tests want the error surfaced.
func (s *Store) AuditAppend(actorKind string, actorID int, action, target, detail string) error {
	_, err := s.db.Exec(
		`INSERT INTO audit_log (actor_kind, actor_id, action, target, detail)
		 VALUES (?, ?, ?, ?, ?)`,
		actorKind, actorID, action, target, detail)
	if err != nil {
		return fmt.Errorf("store: audit append: %w", err)
	}
	return nil
}

// AuditList returns audit entries newest first. limit <= 0 uses 100 and
// limit > 500 is clamped to 500 (the API layer also rejects limit > 500).
func (s *Store) AuditList(limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT id, ts, actor_kind, actor_id, action, target, detail
		 FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: audit list: %w", err)
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.ActorKind, &e.ActorID, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, fmt.Errorf("store: audit list: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
