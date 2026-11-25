package api

import (
	"errors"
	"net/http"
	"time"

	"whatbark/internal/store"
)

// ---------- workflows ----------

// guardEmpIDs ensures the caller may touch the given employee ids.
// Admins bypass; humans must have every empID in their subordinate set.
// On failure it writes a 403 (or 500 for graph errors) and returns false.
func (s *Server) guardEmpIDs(w http.ResponseWriter, r *http.Request, empIDs ...int) bool {
	id := s.caller(r)
	if id.Admin {
		return true
	}
	sub, err := s.subordinatesOf(id.EmpID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	for _, empID := range empIDs {
		if !sub[empID] {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "not your subordinate"})
			return false
		}
	}
	return true
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	wfs, err := s.Store.ListWorkflows()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(wfs))
	for _, wf := range wfs {
		out = append(out, map[string]any{"id": wf.ID, "name": wf.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": out})
}

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if b.Name == "" {
		writeErr(w, http.StatusBadRequest, errNameRequired)
		return
	}
	id, err := s.Store.CreateWorkflow(b.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	wf, err := s.Store.GetWorkflow(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	nodes, err := s.Store.ListWorkflowNodes(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	edges, err := s.Store.ListWorkflowEdges(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	nOut := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		row := map[string]any{"id": n.ID, "employee_id": n.EmpID, "x": n.X, "y": n.Y}
		if emp, err := s.Store.GetEmployee(n.EmpID); err == nil {
			row["employee_name"] = emp.Name
		}
		nOut = append(nOut, row)
	}
	eOut := make([]map[string]any, 0, len(edges))
	for _, e := range edges {
		eOut = append(eOut, map[string]any{"id": e.ID, "from_node_id": e.FromNodeID, "to_node_id": e.ToNodeID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workflow": map[string]any{"id": wf.ID, "name": wf.Name},
		"nodes":    nOut,
		"edges":    eOut,
	})
}

func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.DeleteWorkflow(id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAddWorkflowNode(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var b struct {
		EmpID int     `json:"employee_id"`
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.guardEmpIDs(w, r, b.EmpID) {
		return
	}
	nid, err := s.Store.AddWorkflowNode(id, b.EmpID, b.X, b.Y)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": nid})
}

// handleMoveWorkflowNode persists a dragged node position.
func (s *Server) handleMoveWorkflowNode(w http.ResponseWriter, r *http.Request) {
	nodeID, err := intParam(r, "nodeId")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var b struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	node, err := s.Store.GetWorkflowNode(nodeID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if !s.guardEmpIDs(w, r, node.EmpID) {
		return
	}
	if err := s.Store.MoveWorkflowNode(nodeID, b.X, b.Y); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRemoveWorkflowNode(w http.ResponseWriter, r *http.Request) {
	nodeID, err := intParam(r, "nodeId")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	node, err := s.Store.GetWorkflowNode(nodeID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if !s.guardEmpIDs(w, r, node.EmpID) {
		return
	}
	if err := s.Store.RemoveWorkflowNode(nodeID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAddWorkflowEdge(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var b struct {
		FromNodeID int `json:"from_node_id"`
		ToNodeID   int `json:"to_node_id"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	fromNode, err := s.Store.GetWorkflowNode(b.FromNodeID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	toNode, err := s.Store.GetWorkflowNode(b.ToNodeID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if !s.guardEmpIDs(w, r, fromNode.EmpID, toNode.EmpID) {
		return
	}
	eid, err := s.Store.AddWorkflowEdge(id, b.FromNodeID, b.ToNodeID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": eid})
}

func (s *Server) handleRemoveWorkflowEdge(w http.ResponseWriter, r *http.Request) {
	edgeID, err := intParam(r, "edgeId")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	edge, err := s.Store.GetWorkflowEdge(edgeID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	fromNode, err := s.Store.GetWorkflowNode(edge.FromNodeID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	toNode, err := s.Store.GetWorkflowNode(edge.ToNodeID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if !s.guardEmpIDs(w, r, fromNode.EmpID, toNode.EmpID) {
		return
	}
	if err := s.Store.RemoveWorkflowEdge(edgeID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- projects ----------

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.ListProjects()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		row := map[string]any{"id": p.ID, "name": p.Name, "brief": p.Brief}
		if p.WorkflowID != nil {
			row["workflow_id"] = *p.WorkflowID
		}
		if p.EntryEmpID != nil {
			row["entry_employee_id"] = *p.EntryEmpID
			if emp, err := s.Store.GetEmployee(*p.EntryEmpID); err == nil {
				row["entry_employee_name"] = emp.Name
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name            string `json:"name"`
		Brief           string `json:"brief"`
		WorkflowID      *int   `json:"workflow_id"`
		EntryEmployeeID *int   `json:"entry_employee_id"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if b.Name == "" {
		writeErr(w, http.StatusBadRequest, errNameRequired)
		return
	}
	pid, err := s.Flow.SeedProject(b.Name, b.Brief, b.WorkflowID, b.EntryEmployeeID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": pid})
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.Store.GetProject(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	tasks, err := s.Store.ListTasksByProject(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	tOut := make([]map[string]any, 0, len(tasks))
	now := time.Now().UTC()
	for _, t := range tasks {
		row := map[string]any{
			"id": t.ID, "employee_id": t.EmpID, "kind": t.Kind, "status": t.Status,
			"input": t.Input, "output": t.Output, "requested_by": t.RequestedBy,
			"created_at": t.CreatedAt,
		}
		if sec, label, ok := taskDuration(t, now); ok {
			row["duration_sec"] = sec
			row["duration"] = label
		}
		if t.ProjectID != nil {
			row["project_id"] = *t.ProjectID
		}
		if emp, err := s.Store.GetEmployee(t.EmpID); err == nil {
			row["employee_name"] = emp.Name
		}
		tOut = append(tOut, row)
	}
	row := map[string]any{"id": p.ID, "name": p.Name, "brief": p.Brief}
	if p.WorkflowID != nil {
		row["workflow_id"] = *p.WorkflowID
	}
	if p.EntryEmpID != nil {
		row["entry_employee_id"] = *p.EntryEmpID
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": row, "tasks": tOut})
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.Store.GetProject(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var b struct {
		Brief           *string `json:"brief"`
		WorkflowID      *int    `json:"workflow_id"`
		EntryEmployeeID *int    `json:"entry_employee_id"`
	}
	if err := readJSONOptional(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	brief := p.Brief
	if b.Brief != nil {
		brief = *b.Brief
	}
	wfID := p.WorkflowID
	if b.WorkflowID != nil {
		wfID = b.WorkflowID
	}
	entry := p.EntryEmpID
	if b.EntryEmployeeID != nil {
		entry = b.EntryEmployeeID
	}
	if err := s.Store.UpdateProject(id, brief, wfID, entry); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.DeleteProject(id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- schedules ----------

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	var subMap map[int]bool
	if !id.Admin {
		var err error
		subMap, err = s.subordinatesOf(id.EmpID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	scs, err := s.Store.ListSchedules()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(scs))
	for _, sc := range scs {
		if subMap != nil && !subMap[sc.EmpID] {
			continue
		}
		row := map[string]any{
			"id": sc.ID, "name": sc.Name, "employee_id": sc.EmpID,
			"every_seconds": sc.EverySeconds, "prompt": sc.Prompt, "enabled": sc.Enabled,
		}
		if sc.LastFiredAt != nil {
			row["last_fired_at"] = *sc.LastFiredAt
		}
		if emp, err := s.Store.GetEmployee(sc.EmpID); err == nil {
			row["employee_name"] = emp.Name
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name         string `json:"name"`
		EmpID        int    `json:"employee_id"`
		EverySeconds int    `json:"every_seconds"`
		Prompt       string `json:"prompt"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if b.Name == "" {
		writeErr(w, http.StatusBadRequest, errNameRequired)
		return
	}
	if !s.guardEmpIDs(w, r, b.EmpID) {
		return
	}
	id, err := s.Store.CreateSchedule(b.Name, b.EmpID, b.EverySeconds, b.Prompt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handlePatchSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := s.Store.GetSchedule(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if !s.guardEmpIDs(w, r, sc.EmpID) {
		return
	}
	var b struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.SetScheduleEnabled(id, b.Enabled); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := intParam(r, "id")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := s.Store.GetSchedule(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if !s.guardEmpIDs(w, r, sc.EmpID) {
		return
	}
	if err := s.Store.DeleteSchedule(id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
