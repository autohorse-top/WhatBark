package store

import (
	"testing"
)

// V-3: CreateWFlowGraph creates workflow + nodes + edges atomically.
func TestCreateWFlowGraph(t *testing.T) {
	st, err := Open(t.TempDir() + "/wfg.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	dept, err := st.CreateDepartment("A", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int, 0, 4)
	for _, n := range []string{"E1", "E2", "E3", "E4"} {
		id, err := st.CreateEmployee(n, "role", KindAI, dept)
		if err != nil {
			t.Fatalf("CreateEmployee %s: %v", n, err)
		}
		ids = append(ids, id)
	}

	// Happy path: 3 nodes, 2 edges.
	wfID, err := st.CreateWFlowGraph(WFlowGraphInput{
		Name:  "g-happy",
		Nodes: []WFlowNodeInput{{EmpID: ids[0]}, {EmpID: ids[1], X: 1, Y: 2}, {EmpID: ids[2]}},
		Edges: []WFlowEdgeInput{{FromEmpID: ids[0], ToEmpID: ids[1]}, {FromEmpID: ids[1], ToEmpID: ids[2]}},
	})
	if err != nil {
		t.Fatalf("happy: %v", err)
	}
	if wfID == 0 {
		t.Fatal("happy: workflow id = 0")
	}
	nodes, err := st.ListWorkflowNodes(wfID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(nodes))
	}
	if nodes[1].X != 1 || nodes[1].Y != 2 {
		t.Errorf("node coords = (%v,%v), want (1,2)", nodes[1].X, nodes[1].Y)
	}
	edges, err := st.ListWorkflowEdges(wfID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(edges))
	}
	byNode := make(map[int]int, len(nodes))
	for _, n := range nodes {
		byNode[n.EmpID] = n.ID
	}
	if edges[0].FromNodeID != byNode[ids[0]] || edges[0].ToNodeID != byNode[ids[1]] {
		t.Errorf("edge0 = %v, want %d->%d", edges[0], byNode[ids[0]], byNode[ids[1]])
	}

	// Invalid inputs must reject without writing anything (atomicity).
	wfCount := func() int {
		wfs, _ := st.ListWorkflows()
		return len(wfs)
	}
	before := wfCount()
	cases := []struct {
		name string
		in   WFlowGraphInput
	}{
		{"empty name", WFlowGraphInput{Name: "  ", Nodes: []WFlowNodeInput{{EmpID: ids[0]}, {EmpID: ids[1]}}}},
		{"single node", WFlowGraphInput{Name: "g1", Nodes: []WFlowNodeInput{{EmpID: ids[0]}}}},
		{"unknown employee", WFlowGraphInput{Name: "g2", Nodes: []WFlowNodeInput{{EmpID: ids[0]}, {EmpID: 9999}}}},
		{"duplicate node", WFlowGraphInput{Name: "g3", Nodes: []WFlowNodeInput{{EmpID: ids[0]}, {EmpID: ids[0]}}}},
		{"non-positive emp", WFlowGraphInput{Name: "g4", Nodes: []WFlowNodeInput{{EmpID: 0}, {EmpID: ids[1]}}}},
		{"edge non-node", WFlowGraphInput{Name: "g5", Nodes: []WFlowNodeInput{{EmpID: ids[0]}, {EmpID: ids[1]}}, Edges: []WFlowEdgeInput{{FromEmpID: ids[0], ToEmpID: ids[3]}}}},
		{"self-loop", WFlowGraphInput{Name: "g6", Nodes: []WFlowNodeInput{{EmpID: ids[0]}, {EmpID: ids[1]}}, Edges: []WFlowEdgeInput{{FromEmpID: ids[0], ToEmpID: ids[0]}}}},
		{"duplicate edge", WFlowGraphInput{Name: "g7", Nodes: []WFlowNodeInput{{EmpID: ids[0]}, {EmpID: ids[1]}}, Edges: []WFlowEdgeInput{{FromEmpID: ids[0], ToEmpID: ids[1]}, {FromEmpID: ids[0], ToEmpID: ids[1]}}}},
	}
	for _, c := range cases {
		if _, err := st.CreateWFlowGraph(c.in); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
		if got := wfCount(); got != before {
			t.Errorf("%s: workflow count = %d, want %d (partial write)", c.name, got, before)
		}
	}
}
