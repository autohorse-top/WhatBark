package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"whatbark/internal/hub"
	"whatbark/internal/store"
)

// handleCreateGroup: POST /api/groups {name, member_ids, kind?} (S14; kind
// added in S21). Any authenticated caller (boss or employee) may create a
// group; kind=channel is boss-only (employee gets 403). The creator is
// always added as a member. Returns 201 {id, name, kind}; validation
// failures are 400.
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	var b struct {
		Name      string `json:"name"`
		MemberIDs []int  `json:"member_ids"`
		Kind      string `json:"kind"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// S21: channels are a boss-only surface; the store re-checks this, but
	// answering 403 here keeps the semantics visible at the API edge.
	if b.Kind == "channel" && id.EmpID != 0 {
		writeErr(w, http.StatusForbidden, errors.New("channel: only boss may create"))
		return
	}
	gid, err := s.Store.CreateGroup(b.Name, id.EmpID, b.MemberIDs, b.Kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.Store.GetGroup(gid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.Hub.Broadcast(hub.Event{Type: "group.members", Payload: map[string]any{"group_id": gid}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": gid, "name": g.Name, "kind": g.Kind})
}

// groupCanManage reports whether the caller may manage the group (add
// members): the creator or the boss.
func groupCanManage(id identity, g store.Group) bool {
	return id.EmpID == 0 || id.EmpID == g.CreatedBy
}

// groupView is the shared payload of GET /api/groups/{id} and the
// candidates lookup used by the settings modal (S17).
type groupView struct {
	Group      store.Group
	Members    []map[string]any
	CanManage  bool
	Candidates []map[string]any // only present when CanManage
	Muted      bool
	Ignored    bool
}

// loadGroupView validates membership and assembles the group payload.
func (s *Server) loadGroupView(w http.ResponseWriter, r *http.Request, gid int) (*groupView, error) {
	g, err := s.Store.GetGroup(gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, err
	}
	id := s.caller(r)
	ok, err := s.Store.IsGroupMember(gid, id.EmpID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, err
	}
	if !ok {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return nil, errSelfScope
	}
	canManage := groupCanManage(id, g)
	members, err := s.Store.ListGroupMemberDetails(gid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, err
	}
	mOut := make([]map[string]any, 0, len(members))
	inGroup := map[int]bool{}
	for _, m := range members {
		inGroup[m.EmployeeID] = true
		mOut = append(mOut, map[string]any{
			"employee_id": m.EmployeeID,
			"name":        m.Name,
			"kind":        m.Kind,
			"role":        m.Role,
			"is_creator":  m.IsCreator,
		})
	}
	muted, ignored, err := s.Store.GetGroupPrefs(id.EmpID, gid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, err
	}
	v := &groupView{Group: g, Members: mOut, CanManage: canManage, Muted: muted, Ignored: ignored}
	if canManage {
		cands := []map[string]any{}
		if !inGroup[0] {
			cands = append(cands, map[string]any{"employee_id": 0, "name": "boss"})
		}
		emps, err := s.Store.ListEmployees()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return nil, err
		}
		for _, e := range emps {
			if !inGroup[e.ID] {
				cands = append(cands, map[string]any{"employee_id": e.ID, "name": e.Name})
			}
		}
		v.Candidates = cands
	}
	return v, nil
}

// handleGetGroup: GET /api/groups/{id} (S17). Group members see the group's
// identity, member list and their own mute/ignore prefs. Candidates
// (boss + employees outside the group) are only included for managers.
func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	gid, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	v, err := s.loadGroupView(w, r, gid)
	if err != nil {
		return
	}
	resp := map[string]any{
		"id":                  v.Group.ID,
		"name":                v.Group.Name,
		"kind":                v.Group.Kind, // S21: "group" | "channel"
		"created_at":          v.Group.CreatedAt,
		"created_by":          v.Group.CreatedBy,
		"creator_name":        creatorDisplayName(s.Store, v.Group.CreatedBy),
		"members":             v.Members,
		"can_manage":          v.CanManage,
		"muted":               v.Muted,
		"ignored":             v.Ignored,
		"default_priority":    v.Group.DefaultPriority,
		"sla_timeout_seconds": v.Group.SlaTimeoutSeconds,
		"webhook_url":         v.Group.WebhookURL, // V-4
	}
	if v.CanManage {
		resp["candidates"] = v.Candidates
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAddGroupMembers: POST /api/groups/{id}/members {employee_ids} (S17).
// Only the group creator or the boss may add members.
func (s *Server) handleAddGroupMembers(w http.ResponseWriter, r *http.Request) {
	gid, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.Store.GetGroup(gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	// The boss may manage any group even without being a member.
	if id.EmpID != 0 {
		ok, err := s.Store.IsGroupMember(gid, id.EmpID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, errSelfScope)
			return
		}
	}
	if !groupCanManage(id, g) {
		writeErr(w, http.StatusForbidden, errors.New("only the group creator or boss can add members"))
		return
	}
	var b struct {
		EmployeeIDs []int `json:"employee_ids"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	added, err := s.Store.AddGroupMembers(gid, b.EmployeeIDs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(added) > 0 {
		s.Hub.Broadcast(hub.Event{Type: "group.members", Payload: map[string]any{"group_id": gid, "added": added}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added})
}

// handleSetGroupPrefs: POST /api/groups/{id}/prefs {muted?, ignored?} (S17).
// Any member can update their own prefs; at least one flag is required.
// Responds with the full, updated flag pair.
func (s *Server) handleSetGroupPrefs(w http.ResponseWriter, r *http.Request) {
	gid, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.Store.GetGroup(gid); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	ok, err := s.Store.IsGroupMember(gid, id.EmpID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusForbidden, errSelfScope)
		return
	}
	var b struct {
		Muted   *bool `json:"muted"`
		Ignored *bool `json:"ignored"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if b.Muted == nil && b.Ignored == nil {
		writeErr(w, http.StatusBadRequest, errors.New("at least one of \"muted\" or \"ignored\" required"))
		return
	}
	if err := s.Store.SetGroupPrefs(id.EmpID, gid, b.Muted, b.Ignored); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	muted, ignored, err := s.Store.GetGroupPrefs(id.EmpID, gid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "muted": muted, "ignored": ignored})
}

// handleGroupTasks: GET /api/groups/{id}/tasks?limit=N[&project=P] (S24,
// S29). The group's task board: members (the boss or any group member) see
// the group's tasks, newest first. limit defaults to 50 and must be 1..500
// (else 400). S29: optional project filter — ?project=P with P >= 0 integer;
// P = 0 selects only tasks without a project (project_id IS NULL), P > 0
// only that project's tasks; absent means all (unchanged behavior);
// non-integer or negative -> 400. Row fields are exactly: id, status, input,
// employee_id, employee_name (omitted when the employee no longer exists),
// requested_by (int, always present, 0 = boss — S25, so the client can tell
// "I am the requester"), requested_by_name (0 -> "boss", omitted when the
// requester no longer exists), project_id (S29, always present, null when
// the task has no project), project_name (S29, only when project_id is non-
// null and the project still exists), created_at, message_id (the dispatch
// message id, always present — null when the anchor message is missing,
// making the row non-clickable). Output, kind and priority are intentionally
// not exposed (board = status + jump only).
func (s *Server) handleGroupTasks(w http.ResponseWriter, r *http.Request) {
	gid, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.Store.GetGroup(gid); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	// The boss may read any group's board even without membership (same
	// exemption as handleAddGroupMembers); employees must be members.
	if id.EmpID != 0 {
		ok, err := s.Store.IsGroupMember(gid, id.EmpID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, errSelfScope)
			return
		}
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		n, perr := strconv.Atoi(l)
		if perr != nil || n < 1 || n > 500 {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be an integer in [1,500]"))
			return
		}
		limit = n
	}
	// S29: optional project filter. Absent = all tasks; 0 = only tasks
	// without a project; >0 = only that project's tasks.
	var projectID *int
	if p := r.URL.Query().Get("project"); p != "" {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, errors.New("project must be an integer >= 0"))
			return
		}
		projectID = &n
	}
	tasks, err := s.Store.ListGroupTasks(gid, limit, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// S24 P2-4: batched lookups replace the per-row GetEmployee / GetProject /
	// DispatchMessageIDForGroupTask N+1 pattern (limit=500 used to mean
	// ~3N+1 round trips). Row fields and null semantics are unchanged.
	empIDs := make([]int, 0, len(tasks)*2)
	for _, t := range tasks {
		empIDs = append(empIDs, t.EmpID)
		if t.RequestedBy != 0 {
			empIDs = append(empIDs, t.RequestedBy)
		}
	}
	empNames, err := s.Store.EmployeeNames(empIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var projIDs []int
	for _, t := range tasks {
		if t.ProjectID != nil {
			projIDs = append(projIDs, *t.ProjectID)
		}
	}
	var projNames map[int]string
	if len(projIDs) > 0 {
		projNames, err = s.Store.ProjectNames(projIDs)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	taskIDs := make([]int, len(tasks))
	for i, t := range tasks {
		taskIDs[i] = t.ID
	}
	dispatchIDs, err := s.Store.DispatchMessageIDsForGroupTasks(gid, taskIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	now := time.Now().UTC()
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		row := map[string]any{
			"id": t.ID, "status": t.Status, "input": t.Input,
			"employee_id": t.EmpID, "created_at": t.CreatedAt,
			// S25: requester id (always present, 0 = boss) so the client can
			// apply the cancel-button heuristic; the server's CancelAllowed
			// remains the final permission check.
			"requested_by": t.RequestedBy,
			// S29: project binding (null when the task has no project).
			"project_id": t.ProjectID,
		}
		// Server-computed duration (task-duration-artifacts A).
		if sec, label, ok := taskDuration(t, now); ok {
			row["duration_sec"] = sec
			row["duration"] = label
		}
		if name, ok := empNames[t.EmpID]; ok {
			row["employee_name"] = name
		}
		if t.ProjectID != nil {
			// S29: project name alongside the id (omitted when the project
			// was deleted, same tolerance pattern as employee_name).
			if name, ok := projNames[*t.ProjectID]; ok {
				row["project_name"] = name
			}
		}
		if t.RequestedBy == 0 {
			row["requested_by_name"] = "boss"
		} else if name, ok := empNames[t.RequestedBy]; ok {
			row["requested_by_name"] = name
		}
		// Always present: a number when the anchor exists, null otherwise
		// (the frontend treats null as "row not clickable").
		if mid, ok := dispatchIDs[t.ID]; ok {
			row["message_id"] = mid
		} else {
			row["message_id"] = nil
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

// creatorDisplayName resolves a group creator's display name (0 = "boss").
func creatorDisplayName(s *store.Store, createdBy int) string {
	if createdBy == 0 {
		return "boss"
	}
	if emp, err := s.GetEmployee(createdBy); err == nil {
		return emp.Name
	}
	return "boss"
}

// handleRenameGroup (S26): PATCH /api/groups/{id} {name} — rename a group.
// Only the creator or the boss (groupCanManage); employees must also be
// members. 400 invalid/missing name, 403 scope, 404 unknown group.
func (s *Server) handleRenameGroup(w http.ResponseWriter, r *http.Request) {
	gid, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.Store.GetGroup(gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	if id.EmpID != 0 {
		ok, err := s.Store.IsGroupMember(gid, id.EmpID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, errSelfScope)
			return
		}
	}
	if !groupCanManage(id, g) {
		writeErr(w, http.StatusForbidden, errors.New("only the group creator or boss can rename the group"))
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.RenameGroup(r.Context(), gid, body.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.Hub.Broadcast(hub.Event{Type: "group.updated", Payload: map[string]any{
		"group_id": gid,
		"name":     strings.TrimSpace(body.Name),
	}})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetGroupPolicy (S30): POST /api/groups/{id}/policy
// {default_priority?:int, sla_timeout_seconds?:int}. At least one field
// required. Validations: default_priority 0..99, sla_timeout_seconds
// 0..2592000. Permissions same as rename: boss manages any group; employees
// must be members AND groupCanManage. Unknown group → 404.
func (s *Server) handleSetGroupPolicy(w http.ResponseWriter, r *http.Request) {
	gid, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.Store.GetGroup(gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	if id.EmpID != 0 {
		ok, err := s.Store.IsGroupMember(gid, id.EmpID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, errSelfScope)
			return
		}
	}
	if !groupCanManage(id, g) {
		writeErr(w, http.StatusForbidden, errors.New("only the group creator or boss can set group policy"))
		return
	}
	var b struct {
		DefaultPriority   *int    `json:"default_priority"`
		SlaTimeoutSeconds *int    `json:"sla_timeout_seconds"`
		WebhookURL        *string `json:"webhook_url"` // V-4: nil = leave unchanged, "" = clear
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if b.DefaultPriority == nil && b.SlaTimeoutSeconds == nil && b.WebhookURL == nil {
		writeErr(w, http.StatusBadRequest, errors.New(`at least one of "default_priority", "sla_timeout_seconds" or "webhook_url" required`))
		return
	}
	if b.DefaultPriority != nil && (*b.DefaultPriority < 0 || *b.DefaultPriority > 99) {
		writeErr(w, http.StatusBadRequest, errors.New("default_priority must be an integer in [0,99]"))
		return
	}
	if b.SlaTimeoutSeconds != nil && (*b.SlaTimeoutSeconds < 0 || *b.SlaTimeoutSeconds > 2592000) {
		writeErr(w, http.StatusBadRequest, errors.New("sla_timeout_seconds must be an integer in [0,2592000]"))
		return
	}
	if b.WebhookURL != nil && *b.WebhookURL != "" &&
		!strings.HasPrefix(*b.WebhookURL, "http://") && !strings.HasPrefix(*b.WebhookURL, "https://") {
		writeErr(w, http.StatusBadRequest, errors.New(`webhook_url must start with http:// or https:// (or "" to clear)`))
		return
	}
	if b.DefaultPriority != nil || b.SlaTimeoutSeconds != nil {
		if err := s.Store.SetGroupPolicy(r.Context(), gid, b.DefaultPriority, b.SlaTimeoutSeconds); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, err)
				return
			}
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if b.WebhookURL != nil {
		if err := s.Store.SetGroupWebhook(r.Context(), gid, b.WebhookURL); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, err)
				return
			}
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	// Re-read for the response
	g2, err := s.Store.GetGroup(gid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.Hub.Broadcast(hub.Event{Type: "group.updated", Payload: map[string]any{
		"group_id":            gid,
		"default_priority":    g2.DefaultPriority,
		"sla_timeout_seconds": g2.SlaTimeoutSeconds,
		"webhook_url":         g2.WebhookURL,
	}})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"default_priority":    g2.DefaultPriority,
		"sla_timeout_seconds": g2.SlaTimeoutSeconds,
		"webhook_url":         g2.WebhookURL,
	})
}

// handleRemoveGroupMember (S26): DELETE /api/groups/{id}/members/{eid} —
// remove a member from a group. A member may always remove themselves
// (leave); removing someone else requires manager rights (creator or boss).
// The boss row (eid 0) is protected. 400 <2 members after removal, 403
// scope, 404 unknown group or member not in the group.
func (s *Server) handleRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	gid, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	eid, err := intParam(r, "eid")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.Store.GetGroup(gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	if eid == 0 {
		writeErr(w, http.StatusForbidden, errors.New("the boss cannot be removed from a group"))
		return
	}
	if eid != id.EmpID {
		if id.EmpID != 0 {
			ok, err := s.Store.IsGroupMember(gid, id.EmpID)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if !ok {
				writeErr(w, http.StatusForbidden, errSelfScope)
				return
			}
		}
		if !groupCanManage(id, g) {
			writeErr(w, http.StatusForbidden, errors.New("only the group creator or boss can remove members"))
			return
		}
	}
	inGroup, err := s.Store.IsGroupMember(gid, eid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !inGroup {
		writeErr(w, http.StatusNotFound, fmt.Errorf("member %d is not in group %d", eid, gid))
		return
	}
	if err := s.Store.RemoveGroupMember(r.Context(), gid, eid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.Hub.Broadcast(hub.Event{Type: "group.members", Payload: map[string]any{
		"group_id": gid,
		"removed":  []int{eid},
	}})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteGroup (S26): DELETE /api/groups/{id} — delete a group. Only
// the creator or the boss (groupCanManage); employees must also be members.
// 409 when the group still has open (queued/running) tasks, 403 scope,
// 404 unknown group.
func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	gid, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.Store.GetGroup(gid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := s.caller(r)
	if id.EmpID != 0 {
		ok, err := s.Store.IsGroupMember(gid, id.EmpID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, errSelfScope)
			return
		}
	}
	if !groupCanManage(id, g) {
		writeErr(w, http.StatusForbidden, errors.New("only the group creator or boss can delete the group"))
		return
	}
	open, err := s.Store.OpenTasksByGroup(gid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if open > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("Group still has %d unfinished task(s)", open),
		})
		return
	}
	if err := s.Store.DeleteGroup(r.Context(), gid); err != nil {
		var openErr store.ErrGroupHasOpenTasks
		if errors.As(err, &openErr) {
			// In-transaction recheck (TOCTOU backstop): a concurrent dispatch may
			// land between the pre-check and the delete.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": fmt.Sprintf("Group still has %d unfinished task(s)", openErr.Count),
			})
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.Hub.Broadcast(hub.Event{Type: "group.deleted", Payload: map[string]any{
		"group_id": gid,
	}})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
