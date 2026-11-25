package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Workflow is a named, user-designed collaboration graph over employees.
type Workflow struct {
	ID   int
	Name string
}

// WFlowNode places an employee on a workflow canvas.
type WFlowNode struct {
	ID         int
	WorkflowID int
	EmpID      int
	X, Y       float64
}

// WFlowEdge is a directed connection from one node to another inside a workflow.
type WFlowEdge struct {
	ID         int
	WorkflowID int
	FromNodeID int
	ToNodeID   int
}

// CreateWorkflow inserts a workflow with a unique name.
func (s *Store) CreateWorkflow(name string) (int, error) {
	if name == "" {
		return 0, fmt.Errorf("store: create workflow: name must not be empty")
	}
	res, err := s.db.Exec(`INSERT INTO workflows (name) VALUES (?)`, name)
	if err != nil {
		return 0, fmt.Errorf("store: create workflow %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create workflow %q: %w", name, err)
	}
	return int(id), nil
}

// GetWorkflow returns a workflow by id.
func (s *Store) GetWorkflow(id int) (Workflow, error) {
	var w Workflow
	err := s.db.QueryRow(`SELECT id, name FROM workflows WHERE id = ?`, id).Scan(&w.ID, &w.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	if err != nil {
		return w, fmt.Errorf("store: get workflow %d: %w", id, err)
	}
	return w, nil
}

// ListWorkflows returns all workflows ordered by id.
func (s *Store) ListWorkflows() ([]Workflow, error) {
	rows, err := s.db.Query(`SELECT id, name FROM workflows ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list workflows: %w", err)
	}
	defer rows.Close()
	out := make([]Workflow, 0, 8)
	for rows.Next() {
		var w Workflow
		if err := rows.Scan(&w.ID, &w.Name); err != nil {
			return nil, fmt.Errorf("store: list workflows: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list workflows: %w", err)
	}
	return out, nil
}

// DeleteWorkflow removes a workflow; its nodes and edges are cascaded.
func (s *Store) DeleteWorkflow(id int) error {
	res, err := s.db.Exec(`DELETE FROM workflows WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete workflow %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddWorkflowNode places employee empID on workflow wfID at (x, y).
// An employee may appear at most once per workflow.
func (s *Store) AddWorkflowNode(wfID, empID int, x, y float64) (int, error) {
	if _, err := s.GetWorkflow(wfID); err != nil {
		return 0, fmt.Errorf("store: add node: workflow: %w", err)
	}
	if _, err := s.GetEmployee(empID); err != nil {
		return 0, fmt.Errorf("store: add node: employee: %w", err)
	}
	res, err := s.db.Exec(`
		INSERT INTO wflow_nodes (workflow_id, employee_id, x, y) VALUES (?, ?, ?, ?)`,
		wfID, empID, x, y)
	if err != nil {
		return 0, fmt.Errorf("store: add node (workflow %d, employee %d): %w", wfID, empID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: add node: %w", err)
	}
	return int(id), nil
}

// ListWorkflowNodes returns the nodes of a workflow ordered by id.
func (s *Store) ListWorkflowNodes(wfID int) ([]WFlowNode, error) {
	rows, err := s.db.Query(`
		SELECT id, workflow_id, employee_id, x, y FROM wflow_nodes WHERE workflow_id = ? ORDER BY id`, wfID)
	if err != nil {
		return nil, fmt.Errorf("store: list nodes of workflow %d: %w", wfID, err)
	}
	defer rows.Close()
	out := make([]WFlowNode, 0, 8)
	for rows.Next() {
		var n WFlowNode
		if err := rows.Scan(&n.ID, &n.WorkflowID, &n.EmpID, &n.X, &n.Y); err != nil {
			return nil, fmt.Errorf("store: list nodes of workflow %d: %w", wfID, err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list nodes of workflow %d: %w", wfID, err)
	}
	return out, nil
}

// GetWorkflowNode returns a single workflow node by id.
func (s *Store) GetWorkflowNode(nodeID int) (WFlowNode, error) {
	var n WFlowNode
	err := s.db.QueryRow(`
		SELECT id, workflow_id, employee_id, x, y FROM wflow_nodes WHERE id = ?`, nodeID).
		Scan(&n.ID, &n.WorkflowID, &n.EmpID, &n.X, &n.Y)
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	if err != nil {
		return n, fmt.Errorf("store: get workflow node %d: %w", nodeID, err)
	}
	return n, nil
}

// MoveWorkflowNode updates a node's canvas position (drag persistence).
func (s *Store) MoveWorkflowNode(nodeID int, x, y float64) error {
	res, err := s.db.Exec(`UPDATE wflow_nodes SET x = ?, y = ? WHERE id = ?`, x, y, nodeID)
	if err != nil {
		return fmt.Errorf("store: move node %d: %w", nodeID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RemoveWorkflowNode deletes a node; edges referencing it are cascaded.
func (s *Store) RemoveWorkflowNode(nodeID int) error {
	res, err := s.db.Exec(`DELETE FROM wflow_nodes WHERE id = ?`, nodeID)
	if err != nil {
		return fmt.Errorf("store: remove node %d: %w", nodeID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddWorkflowEdge connects two nodes of the same workflow.
func (s *Store) AddWorkflowEdge(wfID, fromNodeID, toNodeID int) (int, error) {
	if _, err := s.GetWorkflow(wfID); err != nil {
		return 0, fmt.Errorf("store: add edge: workflow: %w", err)
	}
	for _, nodeID := range []int{fromNodeID, toNodeID} {
		var wf int
		if err := s.db.QueryRow(`SELECT workflow_id FROM wflow_nodes WHERE id = ?`, nodeID).Scan(&wf); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("store: add edge: node %d not found", nodeID)
			}
			return 0, fmt.Errorf("store: add edge: %w", err)
		}
		if wf != wfID {
			return 0, fmt.Errorf("store: add edge: node %d belongs to workflow %d, not %d", nodeID, wf, wfID)
		}
	}
	res, err := s.db.Exec(`
		INSERT INTO wflow_edges (workflow_id, from_node_id, to_node_id) VALUES (?, ?, ?)`,
		wfID, fromNodeID, toNodeID)
	if err != nil {
		return 0, fmt.Errorf("store: add edge (workflow %d): %w", wfID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: add edge: %w", err)
	}
	return int(id), nil
}

// ListWorkflowEdges returns the edges of a workflow ordered by id.
func (s *Store) ListWorkflowEdges(wfID int) ([]WFlowEdge, error) {
	rows, err := s.db.Query(`
		SELECT id, workflow_id, from_node_id, to_node_id
		FROM wflow_edges WHERE workflow_id = ? ORDER BY id`, wfID)
	if err != nil {
		return nil, fmt.Errorf("store: list edges of workflow %d: %w", wfID, err)
	}
	defer rows.Close()
	out := make([]WFlowEdge, 0, 8)
	for rows.Next() {
		var e WFlowEdge
		if err := rows.Scan(&e.ID, &e.WorkflowID, &e.FromNodeID, &e.ToNodeID); err != nil {
			return nil, fmt.Errorf("store: list edges of workflow %d: %w", wfID, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list edges of workflow %d: %w", wfID, err)
	}
	return out, nil
}

// GetWorkflowEdge returns a single workflow edge by id.
func (s *Store) GetWorkflowEdge(edgeID int) (WFlowEdge, error) {
	var e WFlowEdge
	err := s.db.QueryRow(`
		SELECT id, workflow_id, from_node_id, to_node_id
		FROM wflow_edges WHERE id = ?`, edgeID).
		Scan(&e.ID, &e.WorkflowID, &e.FromNodeID, &e.ToNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, fmt.Errorf("store: get workflow edge %d: %w", edgeID, err)
	}
	return e, nil
}

// RemoveWorkflowEdge deletes an edge by id.
func (s *Store) RemoveWorkflowEdge(edgeID int) error {
	res, err := s.db.Exec(`DELETE FROM wflow_edges WHERE id = ?`, edgeID)
	if err != nil {
		return fmt.Errorf("store: remove edge %d: %w", edgeID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// WFlowGraphInput is a workflow graph to create atomically (V-3 decompose):
// a named workflow, its employee nodes, and directed edges between them.
type WFlowGraphInput struct {
	Name  string
	Nodes []WFlowNodeInput
	Edges []WFlowEdgeInput
}

// WFlowNodeInput places an employee on the new workflow canvas.
type WFlowNodeInput struct {
	EmpID int
	X, Y  float64
}

// WFlowEdgeInput connects two nodes by their employee ids.
type WFlowEdgeInput struct {
	FromEmpID int
	ToEmpID   int
}

// CreateWFlowGraph creates a workflow plus its nodes and edges in a single
// transaction (V-3). All node employees must exist; edges must reference
// node employees and form no self-loops; duplicate nodes or edges are
// rejected. On any failure nothing is written.
func (s *Store) CreateWFlowGraph(g WFlowGraphInput) (int, error) {
	if strings.TrimSpace(g.Name) == "" {
		return 0, fmt.Errorf("store: create workflow graph: name must not be empty")
	}
	if len(g.Nodes) < 2 {
		return 0, fmt.Errorf("store: create workflow graph: at least 2 nodes required, got %d", len(g.Nodes))
	}
	seen := make(map[int]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.EmpID <= 0 {
			return 0, fmt.Errorf("store: create workflow graph: invalid employee id %d", n.EmpID)
		}
		if seen[n.EmpID] {
			return 0, fmt.Errorf("store: create workflow graph: duplicate node for employee %d", n.EmpID)
		}
		seen[n.EmpID] = true
		if _, err := s.GetEmployee(n.EmpID); err != nil {
			return 0, fmt.Errorf("store: create workflow graph: employee %d: %w", n.EmpID, err)
		}
	}
	edgeKey := func(from, to int) string {
		return fmt.Sprintf("%d->%d", from, to)
	}
	edgeSeen := make(map[string]bool, len(g.Edges))
	for _, e := range g.Edges {
		if e.FromEmpID == e.ToEmpID {
			return 0, fmt.Errorf("store: create workflow graph: self-loop edge for employee %d", e.FromEmpID)
		}
		if !seen[e.FromEmpID] || !seen[e.ToEmpID] {
			return 0, fmt.Errorf("store: create workflow graph: edge references employee %d/%d which is not a node", e.FromEmpID, e.ToEmpID)
		}
		k := edgeKey(e.FromEmpID, e.ToEmpID)
		if edgeSeen[k] {
			return 0, fmt.Errorf("store: create workflow graph: duplicate edge %s", k)
		}
		edgeSeen[k] = true
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: create workflow graph: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO workflows (name) VALUES (?)`, g.Name)
	if err != nil {
		return 0, fmt.Errorf("store: create workflow graph %q: %w", g.Name, err)
	}
	wfID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create workflow graph: %w", err)
	}

	nodeIDs := make(map[int]int, len(g.Nodes))
	for _, n := range g.Nodes {
		res, err := tx.Exec(`
			INSERT INTO wflow_nodes (workflow_id, employee_id, x, y) VALUES (?, ?, ?, ?)`,
			int(wfID), n.EmpID, n.X, n.Y)
		if err != nil {
			return 0, fmt.Errorf("store: create workflow graph: node employee %d: %w", n.EmpID, err)
		}
		nodeID, err := res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("store: create workflow graph: node: %w", err)
		}
		nodeIDs[n.EmpID] = int(nodeID)
	}
	for _, e := range g.Edges {
		if _, err := tx.Exec(`
			INSERT INTO wflow_edges (workflow_id, from_node_id, to_node_id) VALUES (?, ?, ?)`,
			int(wfID), nodeIDs[e.FromEmpID], nodeIDs[e.ToEmpID]); err != nil {
			return 0, fmt.Errorf("store: create workflow graph: edge %d->%d: %w", e.FromEmpID, e.ToEmpID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: create workflow graph: commit: %w", err)
	}
	return int(wfID), nil
}
