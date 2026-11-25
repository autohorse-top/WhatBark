package api

import (
	"errors"
	"net/http"
	"strconv"

	"whatbark/internal/store"
)

// V-2: multi-admin token management (admin-only, H-7 audited with the
// specific admin id). The plaintext token is returned exactly once, by the
// create endpoint; only sha256 digests are stored.

// handleListAdminTokens: GET /api/admin/tokens -> 200 {tokens: [{id,name,created_at}]}.
// No hashes, no plaintext — safe to render in a settings UI.
func (s *Server) handleListAdminTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListAdminTokens()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": rows})
}

// handleCreateAdminToken: POST /api/admin/tokens {name} -> 201
// {id,name,token,created_at}. 400 on invalid or duplicate name. The
// plaintext token is shown exactly once.
func (s *Server) handleCreateAdminToken(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	row, plain, err := s.Store.CreateAdminToken(b.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if cw, ok := w.(*statusWriter); ok {
		cw.SetAuditDetail("name=" + row.Name)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         row.ID,
		"name":       row.Name,
		"token":      plain,
		"created_at": row.CreatedAt,
	})
}

// handleDeleteAdminToken: DELETE /api/admin/tokens/{id} -> 200 {deleted:true}.
// The token stops authenticating immediately (live table lookup). 404 for
// unknown ids.
func (s *Server) handleDeleteAdminToken(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.DeleteAdminToken(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if cw, ok := w.(*statusWriter); ok {
		cw.SetAuditDetail("id=" + strconv.Itoa(id))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}
