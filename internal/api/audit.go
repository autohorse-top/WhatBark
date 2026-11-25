package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// H-7: audit log.

// handleAudit: GET /api/audit?limit=N (admin only) — newest first.
// limit: absent -> 100; present but not in 1..500 -> 400 (the store clamps
// the same range as a backstop).
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 500 {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be 1..500"))
			return
		}
		limit = n
	}
	items, err := s.Store.AuditList(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// auditCall records an audit row for the current caller (admin or employee).
// Used by the task operations that any authorized caller may trigger:
// cancel / preempt / reassign (H-7). Fail-soft by design.
func (s *Server) auditCall(r *http.Request, action string, taskID int, detail string) {
	id := s.caller(r)
	kind, aid := "employee", id.EmpID
	if id.Admin {
		kind, aid = "admin", 0
	}
	_ = s.Store.AuditAppend(kind, aid, action, fmt.Sprintf("/api/tasks/%d", taskID), detail)
}
