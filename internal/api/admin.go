package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"whatbark/internal/flow"
	"whatbark/internal/hub"
	"whatbark/internal/store"
)

// handleMe returns the identity of the caller (A14). Includes the caller's
// avatar URL when set (S12): "/api/avatars/<stored_name>" or absent.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	if id.Admin {
		row := map[string]any{"admin": true, "employee_id": 0, "admin_id": id.AdminID, "subordinates": []any{}}
		if at, err := s.Store.GetAdminToken(id.AdminID); err == nil {
			row["admin_name"] = at.Name
		}
		if boss := s.Store.GetSettingDefault("boss_avatar", ""); boss != "" {
			row["avatar"] = s.signFileURL("/api/avatars/" + boss)
		}
		writeJSON(w, http.StatusOK, row)
		return
	}
	emp, err := s.Store.GetEmployee(id.EmpID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	// Route B: expose the caller's transitive subordinates so the frontend can
	// gate the wflow/sched tabs. Only kind=human employees ever manage
	// subordinates (adminOrHuman blocks AI on the write endpoints), so AI
	// callers get an empty list per contract. On a graph error (e.g.
	// reporting cycle) we fail open to an empty list rather than breaking
	// /api/me at boot.
	sub := []int{}
	if emp.Kind == store.KindHuman {
		if sm, err := s.subordinatesOf(emp.ID); err == nil {
			for id := range sm {
				sub = append(sub, id)
			}
		}
	}
	sort.Ints(sub)
	empRow := map[string]any{
		"id": emp.ID, "name": emp.Name, "role": emp.Role, "kind": emp.Kind,
		"department_id": emp.DepartmentID, "status": s.statusOf(emp),
		"tool": emp.Tool, "tool_args": emp.ToolArgs, "workdir": emp.Workdir,
	}
	if emp.Avatar != "" {
		empRow["avatar"] = s.signFileURL("/api/avatars/" + emp.Avatar)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"admin":        false,
		"employee_id":  emp.ID,
		"employee":     empRow,
		"subordinates": sub,
	})
}

// ---------- departments ----------

func (s *Server) handleListDepartments(w http.ResponseWriter, r *http.Request) {
	depts, err := s.Store.ListDepartments()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(depts))
	for _, d := range depts {
		// S31: group_id is the department's own channel (null for legacy
		// departments created before S31).
		row := map[string]any{"id": d.ID, "name": d.Name, "parent_id": d.ParentID, "group_id": d.GroupID}
		if d.LeaderID != nil {
			if emp, err := s.Store.GetEmployee(*d.LeaderID); err == nil {
				row["leader_id"] = emp.ID
				row["leader_name"] = emp.Name
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"departments": out})
}

// handleCreateDepartment (S31): POST /api/departments {name, parent_id?}.
// Creating a department now also creates its channel (kind=channel, boss as
// creator and sole initial member) in one store transaction. 201 returns
// {id, group_id}; validation failures are 400. On success a group.members
// broadcast makes the new channel appear in the boss's conversation list.
func (s *Server) handleCreateDepartment(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name     string `json:"name"`
		ParentID *int   `json:"parent_id"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id, gid, err := s.Store.CreateDepartmentWithChannel(b.Name, b.ParentID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.Hub.Broadcast(hub.Event{Type: "group.members", Payload: map[string]any{"group_id": gid}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "group_id": gid})
}

// handleRenameDepartment (S31): PUT /api/departments/{id} {name}. Admin-only
// (routed via adminOnly). Renames the department and its channel (if any)
// atomically (H-3: store-level transaction). A group.updated broadcast
// keeps clients in sync. 200 {ok, id, name, group_id}; 400
// validation/duplicate name; 404 unknown department; 409 when the channel
// was concurrently deleted (the store rolled the rename back).
func (s *Server) handleRenameDepartment(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var b struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	d, err := s.Store.RenameDepartment(id, b.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// H-3: the store rolled the rename back because either the
			// department or its channel row vanished concurrently. The
			// department still exists -> the channel was deleted (409);
			// otherwise the department itself is gone (404).
			if _, derr := s.Store.GetDepartment(id); derr == nil {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error": "Department channel was concurrently deleted, please refresh and retry",
				})
				return
			}
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if d.GroupID != nil {
		s.Hub.Broadcast(hub.Event{Type: "group.updated", Payload: map[string]any{
			"group_id": *d.GroupID,
			"name":     d.Name,
		}})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "name": d.Name, "group_id": d.GroupID,
	})
}

func (s *Server) handleSetDepartmentLeader(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var b struct {
		EmpID int `json:"employee_id"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.SetDepartmentLeader(id, b.EmpID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteDepartment (S31): DELETE /api/departments/{id}. The store
// method also deletes the department's channel in one transaction (open-task
// re-check included). ErrInUse (child departments/employees) -> 400 (as
// before), ErrGroupHasOpenTasks -> 409 (same wording as handleDeleteGroup),
// ErrNotFound -> 404, others -> 500. A deleted channel broadcasts
// group.deleted.
func (s *Server) handleDeleteDepartment(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	gid, err := s.Store.DeleteDepartmentAndChannel(id)
	if err != nil {
		var openErr store.ErrGroupHasOpenTasks
		switch {
		case errors.As(err, &openErr):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": fmt.Sprintf("Group still has %d unfinished task(s)", openErr.Count),
			})
		case errors.Is(err, store.ErrInUse):
			writeErr(w, http.StatusBadRequest, err)
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	if gid != nil {
		s.Hub.Broadcast(hub.Event{Type: "group.deleted", Payload: map[string]any{"group_id": *gid}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- employees ----------

func (s *Server) handleListEmployees(w http.ResponseWriter, r *http.Request) {
	emps, err := s.Store.ListEmployees()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// tool/tool_args/workdir are deployment details: only the boss sees them
	// in the directory listing (S7-Minor4). An employee's own details remain
	// available via /api/me.
	admin := s.caller(r).Admin
	out := make([]map[string]any, 0, len(emps))
	for _, e := range emps {
		row := map[string]any{
			"id": e.ID, "name": e.Name, "role": e.Role, "kind": e.Kind,
			"department_id": e.DepartmentID, "status": s.statusOf(e),
			"email": e.Email,
		}
		if e.Avatar != "" {
			row["avatar"] = s.signFileURL("/api/avatars/" + e.Avatar)
		}
		if admin {
			row["tool"] = e.Tool
			row["tool_args"] = e.ToolArgs
			row["workdir"] = e.Workdir
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"employees": out})
}

func (s *Server) handleCreateEmployee(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name         string `json:"name"`
		Role         string `json:"role"`
		Kind         string `json:"kind"`
		DepartmentID int    `json:"department_id"`
		Email        string `json:"email"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// V-4: optional notification mailbox, at most 254 characters.
	if len(b.Email) > 254 {
		writeErr(w, http.StatusBadRequest, errors.New("email must be at most 254 characters"))
		return
	}
	id, err := s.Store.CreateEmployee(b.Name, b.Role, b.Kind, b.DepartmentID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// S31: auto-join the new employee to their department's channel. Legacy
	// departments have no channel (GroupID nil) and are skipped; "already a
	// member" is a no-op success. When a member actually joins, broadcast
	// group.members so the channel's conversation list stays in sync.
	if b.DepartmentID > 0 {
		if dept, derr := s.Store.GetDepartment(b.DepartmentID); derr == nil && dept.GroupID != nil {
			added, addErr := s.Store.AddGroupMembers(*dept.GroupID, []int{id})
			if addErr != nil && !errors.Is(addErr, store.ErrNoNewGroupMembers) {
				writeErr(w, http.StatusInternalServerError, addErr)
				return
			}
			if len(added) > 0 {
				s.Hub.Broadcast(hub.Event{Type: "group.members", Payload: map[string]any{
					"group_id": *dept.GroupID,
					"added":    added,
				}})
			}
		}
	}
	tok, err := newToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sum := sha256.Sum256([]byte(tok))
	if err := s.Store.SetTokenHash(id, hex.EncodeToString(sum[:])); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// V-4: persist the optional notification mailbox before responding.
	if b.Email != "" {
		if err := s.Store.SetEmail(id, b.Email); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "token": tok, "email": b.Email})
}

func (s *Server) handleSetEmployeeTool(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var b struct {
		Tool     string `json:"tool"`
		ToolArgs string `json:"tool_args"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.SetTool(id, b.Tool, b.ToolArgs); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReissueEmployeeTokens: POST /api/employees/reissue-tokens (admin only).
// Batch token reissue: for each selected employee a fresh 32-hex token is
// generated and its token_hash replaced, invalidating the old token
// immediately. The DB stores only sha256 digests (plaintext is never
// persisted, same model as admin tokens), so this is the only batch
// retrieval channel for employee tokens. The response returns each new
// plaintext exactly once. Wrapped in adminOnly, which records the audit row
// on 2xx; the ids list is attached as the audit detail. Partial success is
// possible: a failure on a later id does not roll back earlier reissues.
func (s *Server) handleReissueEmployeeTokens(w http.ResponseWriter, r *http.Request) {
	var b struct {
		IDs []int `json:"ids"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(b.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("ids must not be empty"))
		return
	}
	if len(b.IDs) > 100 {
		writeErr(w, http.StatusBadRequest, errors.New("at most 100 ids per request"))
		return
	}
	seen := make(map[int]bool, len(b.IDs))
	tokens := map[string]string{}
	var reissued []int
	for _, id := range b.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		emp, err := s.Store.GetEmployee(id)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("employee id %d not found", id))
			return
		}
		tok, err := newToken()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		sum := sha256.Sum256([]byte(tok))
		if err := s.Store.SetTokenHash(id, hex.EncodeToString(sum[:])); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		tokens[emp.Name] = tok
		reissued = append(reissued, id)
	}
	if cw, ok := w.(*statusWriter); ok {
		cw.SetAuditDetail(fmt.Sprintf("ids=[%v]", reissued))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (s *Server) handleDeleteEmployee(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// S12: clean up the employee's avatar and uploaded files before the row
	// goes (files.owner_id is a loose reference, so it does not cascade).
	if emp, gerr := s.Store.GetEmployee(id); gerr == nil {
		if emp.Avatar != "" {
			_ = os.Remove(filepath.Join(s.Store.FilesDir, "avatars", filepath.Base(emp.Avatar)))
		}
		if fs, ferr := s.Store.ListFilesByOwner(id); ferr == nil {
			for _, f := range fs {
				_ = s.Store.DeleteFile(f.ID)
				_ = os.Remove(filepath.Join(s.Store.FilesDir, "files", filepath.Base(f.StoredName)))
			}
		}
	}
	if err := s.Store.DeleteEmployee(id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- routes ----------

// handleRoutes returns the effective routes for an employee (A1).
// query: employee_id (optional; must be self for non-admin).
func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	target := id.EmpID
	if q := r.URL.Query().Get("employee_id"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if !id.Admin && n != id.EmpID {
			writeErr(w, http.StatusForbidden, errSelfScope)
			return
		}
		target = n
	}
	g, err := flow.NewGraph(s.Store)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// reporting: up-chain in proximity order (report/summary targets).
	reporting := g.Up[target]
	if reporting == nil {
		reporting = []int{}
	}
	// S11d: derive dispatch/chat from the same Allowed() predicate that
	// Flow.Send enforces, so the endpoint never advertises a send the
	// server will reject with 403. Previously chat listed every leader
	// even for non-leader callers; dispatch missed WEdges peers.
	dispatch := []int{}
	chat := []int{}
	if emps, err := s.Store.ListEmployees(); err == nil {
		for _, e := range emps {
			if e.ID == target || e.ID <= 0 {
				continue
			}
			if g.Allowed(target, e.ID, store.MsgDispatch) {
				dispatch = append(dispatch, e.ID)
			}
			if g.Allowed(target, e.ID, store.MsgChat) {
				chat = append(chat, e.ID)
			}
		}
	}
	sort.Ints(dispatch)
	sort.Ints(chat)
	writeJSON(w, http.StatusOK, map[string]any{
		"employee_id": target,
		"reporting":   reporting,
		"dispatch":    dispatch,
		"chat":        chat,
	})
}
