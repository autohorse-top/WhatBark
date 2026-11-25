package api

// V-3: a lead (kind=ai employee) decomposes a boss request into a workflow
// graph (nodes = employees, edges = dependencies) plus optional initial
// dispatch tasks. The lead's agent (e.g. claudecode) generates the JSON and
// submits it via `whatbark bark decompose --data '<json>'`.
//
// POST /api/decompose  (employee token; admin → 403, kind≠ai → 403)
//
//	{
//	  "name":  "release",                                   // 1..100 chars
//	  "nodes": [{"employee_id": 2, "x": 0, "y": 0}, ...],  // 2..32 unique, dispatch-reachable
//	  "edges": [{"from": 2, "to": 5}],                     // reference node employee ids
//	  "tasks": [{"employee_id": 2, "input": "..."}]        // optional initial tasks
//	}
//
// 201 → {"workflow_id", "name", "nodes", "edges", "tasks"}

import (
	"net/http"
	"strconv"
	"strings"

	"whatbark/internal/store"
)

const decomposeMaxNodes = 32

type decomposeNodeReq struct {
	EmpID int     `json:"employee_id"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

type decomposeEdgeReq struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type decomposeTaskReq struct {
	EmpID int    `json:"employee_id"`
	Input string `json:"input"`
}

type decomposeReq struct {
	Name  string             `json:"name"`
	Nodes []decomposeNodeReq `json:"nodes"`
	Edges []decomposeEdgeReq `json:"edges"`
	Tasks []decomposeTaskReq `json:"tasks"`
}

func (s *Server) handleDecompose(w http.ResponseWriter, r *http.Request) {
	id := s.caller(r)
	if id.Admin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin tokens cannot decompose; use a kind=ai employee token"})
		return
	}
	var b decomposeReq
	if err := readJSON(r, &b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	b.Name = strings.TrimSpace(b.Name)

	emp, err := s.Store.GetEmployee(id.EmpID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if emp.Kind != store.KindAI {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "decompose requires a kind=ai employee"})
		return
	}

	if b.Name == "" || len(b.Name) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name must be 1..100 chars"})
		return
	}
	if len(b.Nodes) < 2 || len(b.Nodes) > decomposeMaxNodes {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nodes must contain 2.." + strconv.Itoa(decomposeMaxNodes) + " entries"})
		return
	}
	seen := make(map[int]bool, len(b.Nodes))
	for _, n := range b.Nodes {
		if n.EmpID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nodes[].employee_id must be a positive id"})
			return
		}
		if seen[n.EmpID] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "duplicate node for employee " + strconv.Itoa(n.EmpID)})
			return
		}
		seen[n.EmpID] = true
	}
	// Every node must be dispatch-reachable from the caller (direct
	// subordinates or existing workflow peers); this also rejects self-nodes.
	g, err := s.Flow.Graph()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, n := range b.Nodes {
		if !g.Allowed(id.EmpID, n.EmpID, "dispatch") {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "cannot decompose onto employee " + strconv.Itoa(n.EmpID) + ": not in your dispatch route"})
			return
		}
	}
	for _, e := range b.Edges {
		if e.From == e.To {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "self-loop edge for employee " + strconv.Itoa(e.From)})
			return
		}
		if !seen[e.From] || !seen[e.To] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "edge references employee " + strconv.Itoa(e.From) + " -> " + strconv.Itoa(e.To) + ", both must be nodes"})
			return
		}
	}
	for _, t := range b.Tasks {
		if !seen[t.EmpID] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "task references employee " + strconv.Itoa(t.EmpID) + " which is not a node"})
			return
		}
		if strings.TrimSpace(t.Input) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tasks[].input must not be empty"})
			return
		}
	}

	wfID, err := s.Store.CreateWFlowGraph(store.WFlowGraphInput{
		Name:  b.Name,
		Nodes: nodeInputs(b.Nodes),
		Edges: edgeInputs(b.Edges),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// Initial dispatch tasks (all pre-validated above). The workflow id is
	// linked so the task board shows them under the new graph.
	tasks := make([]map[string]any, 0, len(b.Tasks))
	for _, t := range b.Tasks {
		tid, err := s.Store.CreateTask(t.EmpID, store.KindDispatch, t.Input, nil, &wfID, id.EmpID, 0, nil)
		if err != nil {
			_ = s.Store.DeleteWorkflow(wfID) // keep the API all-or-nothing
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		tasks = append(tasks, map[string]any{"id": tid, "employee_id": t.EmpID, "status": store.TaskQueued})
	}

	nodes, err := s.Store.ListWorkflowNodes(wfID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	edges, err := s.Store.ListWorkflowEdges(wfID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	outNodes := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		outNodes = append(outNodes, map[string]any{"id": n.ID, "employee_id": n.EmpID, "x": n.X, "y": n.Y})
	}
	outEdges := make([]map[string]any, 0, len(edges))
	nodeEmp := make(map[int]int, len(nodes))
	for _, n := range nodes {
		nodeEmp[n.ID] = n.EmpID
	}
	for _, e := range edges {
		outEdges = append(outEdges, map[string]any{"id": e.ID, "from_employee_id": nodeEmp[e.FromNodeID], "to_employee_id": nodeEmp[e.ToNodeID]})
	}

	_ = s.Store.AuditAppend("employee", id.EmpID, "decompose.create", "/api/decompose",
		"workflow="+strconv.Itoa(wfID)+" name="+b.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"workflow_id": wfID,
		"name":        b.Name,
		"nodes":       outNodes,
		"edges":       outEdges,
		"tasks":       tasks,
	})
}

func nodeInputs(ns []decomposeNodeReq) []store.WFlowNodeInput {
	out := make([]store.WFlowNodeInput, 0, len(ns))
	for _, n := range ns {
		out = append(out, store.WFlowNodeInput{EmpID: n.EmpID, X: n.X, Y: n.Y})
	}
	return out
}

func edgeInputs(es []decomposeEdgeReq) []store.WFlowEdgeInput {
	out := make([]store.WFlowEdgeInput, 0, len(es))
	for _, e := range es {
		out = append(out, store.WFlowEdgeInput{FromEmpID: e.From, ToEmpID: e.To})
	}
	return out
}
