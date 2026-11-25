package preset

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"whatbark/internal/store"
)

const validPreset = `{
  "schema": "whatbark-preset/v1",
  "name": "测试组织",
  "departments": [
    {"name": "总经办", "leader": "boss-li", "employees": [
      {"name": "boss-li", "role": "负责人"}
    ]}
  ]
}`

func mustParse(t *testing.T, data string) *Preset {
	t.Helper()
	p, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func freshStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dbPath
}

// TestParseValidation is a table of documents that must be rejected, plus
// the happy paths (valid minimal, parent reference, kind human).
func TestParseValidation(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"invalid json", `{"name": "x", "departments": [`},
		{"unsupported schema", `{"schema": "whatbark-preset/v9", "name": "x", "departments": [{"name": "A", "leader": "a1", "employees": [{"name": "a1", "role": "r"}]}]}`},
		{"empty name", `{"departments": [{"name": "A", "leader": "a1", "employees": [{"name": "a1", "role": "r"}]}]}`},
		{"no departments", `{"name": "x", "departments": []}`},
		{"empty department name", `{"name": "x", "departments": [{"name": "", "leader": "a1", "employees": [{"name": "a1", "role": "r"}]}]}`},
		{"duplicate department", `{"name": "x", "departments": [{"name": "A", "leader": "a1", "employees": [{"name": "a1", "role": "r"}]}, {"name": "A", "leader": "a2", "employees": [{"name": "a2", "role": "r"}]}]}`},
		{"parent forward reference", `{"name": "x", "departments": [{"name": "Child", "parent": "Parent", "leader": "a1", "employees": [{"name": "a1", "role": "r"}]}, {"name": "Parent", "leader": "a2", "employees": [{"name": "a2", "role": "r"}]}]}`},
		{"parent missing", `{"name": "x", "departments": [{"name": "Child", "parent": "Nope", "leader": "a1", "employees": [{"name": "a1", "role": "r"}]}]}`},
		{"department without employees", `{"name": "x", "departments": [{"name": "A", "leader": "a1", "employees": []}]}`},
		{"employee name with space", `{"name": "x", "departments": [{"name": "A", "leader": "a 1", "employees": [{"name": "a 1", "role": "r"}]}]}`},
		{"employee name CJK", `{"name": "x", "departments": [{"name": "A", "leader": "甲", "employees": [{"name": "甲", "role": "r"}]}]}`},
		{"employee name too long", `{"name": "x", "departments": [{"name": "A", "leader": "` + strings.Repeat("a", 65) + `", "employees": [{"name": "` + strings.Repeat("a", 65) + `", "role": "r"}]}]}`},
		{"empty role", `{"name": "x", "departments": [{"name": "A", "leader": "a1", "employees": [{"name": "a1", "role": "  "}]}]}`},
		{"unsupported kind", `{"name": "x", "departments": [{"name": "A", "leader": "a1", "employees": [{"name": "a1", "role": "r", "kind": "daemon"}]}]}`},
		{"duplicate employee across depts", `{"name": "x", "departments": [{"name": "A", "leader": "a1", "employees": [{"name": "a1", "role": "r"}]}, {"name": "B", "leader": "b1", "employees": [{"name": "a1", "role": "r2"}]}]}`},
		{"leader not in employees", `{"name": "x", "departments": [{"name": "A", "leader": "ghost", "employees": [{"name": "a1", "role": "r"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.doc)); err == nil {
				t.Fatalf("Parse accepted %s document", tc.name)
			}
		})
	}

	t.Run("valid minimal", func(t *testing.T) {
		p := mustParse(t, validPreset)
		if len(p.Departments) != 1 || p.Departments[0].Leader != "boss-li" {
			t.Fatalf("unexpected preset: %+v", p)
		}
	})
	t.Run("valid parent reference", func(t *testing.T) {
		doc := `{
		  "name": "x",
		  "departments": [
		    {"name": "总办", "leader": "top", "employees": [{"name": "top", "role": "r"}]},
		    {"name": "分部", "parent": "总办", "leader": "sub", "employees": [{"name": "sub", "role": "r"}]}
		  ]
		}`
		mustParse(t, doc)
	})
	t.Run("valid kind human", func(t *testing.T) {
		doc := `{"name": "x", "departments": [{"name": "A", "leader": "a1", "employees": [{"name": "a1", "role": "r", "kind": "human"}]}]}`
		mustParse(t, doc)
	})
}

func TestImportAndVerify(t *testing.T) {
	st, dbPath := freshStore(t)
	data, _ := Get("software")
	p, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse software: %v", err)
	}
	rep, err := Import(st, p, dbPath)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(rep.Departments) != 5 || rep.Employees != 16 {
		t.Fatalf("import counts: %d depts, %d employees", len(rep.Departments), rep.Employees)
	}
	emps, err := st.ListEmployees()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range emps {
		if e.TokenHash == "" {
			t.Fatalf("employee %s has no token hash", e.Name)
		}
	}
	raw, err := os.ReadFile(rep.TokensFile)
	if err != nil {
		t.Fatalf("tokens file: %v", err)
	}
	var tokens map[string]string
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatalf("tokens file JSON: %v", err)
	}
	if len(tokens) != 16 {
		t.Fatalf("tokens file entries: %d, want 16", len(tokens))
	}
	if len(rep.Tokens) != 16 {
		t.Fatalf("report tokens: %d, want 16", len(rep.Tokens))
	}
	if info, err := os.Stat(rep.TokensFile); err == nil {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("tokens file mode %o, want 0600", perm)
		}
	}
	if err := Verify(st, p); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestImportAllEmbeddedPresets(t *testing.T) {
	wantCounts := map[string]struct{ depts, emps int }{
		"software":  {5, 16},
		"finance":   {5, 14},
		"law":       {5, 14},
		"insurance": {5, 14},
	}
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			want, ok := wantCounts[name]
			if !ok {
				t.Fatalf("unexpected embedded preset %q (update wantCounts)", name)
			}
			data, _ := Get(name)
			p, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			st, dbPath := freshStore(t)
			rep, err := Import(st, p, dbPath)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if len(rep.Departments) != want.depts || rep.Employees != want.emps {
				t.Fatalf("counts: %d depts / %d employees, want %d / %d",
					len(rep.Departments), rep.Employees, want.depts, want.emps)
			}
			if err := Verify(st, p); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

func TestImportCollision(t *testing.T) {
	st, dbPath := freshStore(t)
	data, _ := Get("software")
	p, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(st, p, dbPath); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if _, err := Import(st, p, dbPath); err == nil {
		t.Fatal("second import succeeded, want department-name collision error")
	} else if !strings.Contains(err.Error(), "Design Dept") {
		t.Fatalf("collision error does not name the department: %v", err)
	}
}

// TestImportCustomFile covers a two-level parent/child tree imported from
// raw JSON (the `preset import -file` path): member chains must climb
// through the parent department leader.
func TestImportCustomFile(t *testing.T) {
	doc := `{
	  "name": "两层组织",
	  "departments": [
	    {"name": "总部", "leader": "hq-lead", "employees": [
	      {"name": "hq-lead", "role": "总部负责人"},
	      {"name": "hq-staff", "role": "总部专员"}
	    ]},
	    {"name": "分部", "parent": "总部", "leader": "branch-lead", "employees": [
	      {"name": "branch-lead", "role": "分部负责人"},
	      {"name": "branch-staff", "role": "分部专员", "kind": "human"}
	    ]}
	  ]
	}`
	st, dbPath := freshStore(t)
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Import(st, p, dbPath); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if err := Verify(st, p); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestEnsureTokensIdempotent(t *testing.T) {
	st, dbPath := freshStore(t)
	data, _ := Get("software")
	p, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(st, p, dbPath); err != nil {
		t.Fatal(err)
	}
	before, err := st.ListEmployees()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureTokens(st, dbPath); err != nil {
		t.Fatalf("second EnsureTokens: %v", err)
	}
	after, err := st.ListEmployees()
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range after {
		if e.TokenHash != before[i].TokenHash {
			t.Fatalf("token hash changed for %s on re-run", e.Name)
		}
	}
	raw, err := os.ReadFile(strings.TrimSuffix(dbPath, ".db") + "-tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var tokens map[string]string
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 16 {
		t.Fatalf("tokens entries: %d, want 16", len(tokens))
	}
}

// TestEmbeddedPresetsWorkflows asserts the leadership collab graph of every
// embedded preset (S2): exactly 1 workflow named "Leadership Collab", 5
// leader nodes, and a K5-complete set of 20 directed edges (every ordered
// pair of distinct leaders exactly once). Import must produce the same
// graph in the database — 1 workflow / 5 nodes / 20 edges with node order
// and canvas coordinates matching the spec — and Verify must pass.
func TestEmbeddedPresetsWorkflows(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			data, _ := Get(name)
			p, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse %s: %v", name, err)
			}
			if len(p.Workflows) != 1 {
				t.Fatalf("workflows: %d, want 1", len(p.Workflows))
			}
			spec := p.Workflows[0]
			if spec.Name != "Leadership Collab" {
				t.Fatalf("workflow name %q, want %q", spec.Name, "Leadership Collab")
			}
			if len(spec.Nodes) != 5 {
				t.Fatalf("workflow nodes: %d, want 5", len(spec.Nodes))
			}
			if len(spec.Edges) != 20 {
				t.Fatalf("workflow edges: %d, want 20", len(spec.Edges))
			}
			// K5 completeness: every ordered pair of distinct nodes
			// appears exactly once (10 unordered pairs x 2 directions).
			pairCount := map[[2]string]int{}
			for _, e := range spec.Edges {
				pairCount[[2]string{e[0], e[1]}]++
			}
			if len(pairCount) != 20 {
				t.Fatalf("distinct edge pairs: %d, want 20", len(pairCount))
			}
			for _, a := range spec.Nodes {
				for _, b := range spec.Nodes {
					if a == b {
						continue
					}
					if got := pairCount[[2]string{a, b}]; got != 1 {
						t.Fatalf("pair %s->%s appears %d times, want exactly 1", a, b, got)
					}
				}
			}

			st, dbPath := freshStore(t)
			rep, err := Import(st, p, dbPath)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if len(rep.Workflows) != 1 || rep.Workflows["Leadership Collab"] == 0 {
				t.Fatalf("report workflows: %v", rep.Workflows)
			}
			wfs, err := st.ListWorkflows()
			if err != nil {
				t.Fatal(err)
			}
			if len(wfs) != 1 || wfs[0].Name != "Leadership Collab" {
				t.Fatalf("db workflows: %+v", wfs)
			}
			if wfs[0].ID != rep.Workflows["Leadership Collab"] {
				t.Fatalf("workflow id: db %d, report %d", wfs[0].ID, rep.Workflows["Leadership Collab"])
			}
			emps, err := st.ListEmployees()
			if err != nil {
				t.Fatal(err)
			}
			empID, nameByID := map[string]int{}, map[int]string{}
			for _, e := range emps {
				empID[e.Name] = e.ID
				nameByID[e.ID] = e.Name
			}
			nodes, err := st.ListWorkflowNodes(wfs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(nodes) != 5 {
				t.Fatalf("db nodes: %d, want 5", len(nodes))
			}
			nodeEmp := map[int]int{} // node id -> employee id
			for i, n := range nodes {
				if got := nameByID[n.EmpID]; got != spec.Nodes[i] {
					t.Fatalf("node[%d]: employee %q, want %q", i, got, spec.Nodes[i])
				}
				if n.X != float64(80+i*160) {
					t.Fatalf("node[%d]: x = %v, want %v", i, n.X, float64(80+i*160))
				}
				if n.Y != 200 {
					t.Fatalf("node[%d]: y = %v, want 200", i, n.Y)
				}
				nodeEmp[n.ID] = n.EmpID
			}
			edges, err := st.ListWorkflowEdges(wfs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(edges) != 20 {
				t.Fatalf("db edges: %d, want 20", len(edges))
			}
			gotPairs := map[[2]int]int{}
			for _, e := range edges {
				gotPairs[[2]int{nodeEmp[e.FromNodeID], nodeEmp[e.ToNodeID]}]++
			}
			for _, e := range spec.Edges {
				k := [2]int{empID[e[0]], empID[e[1]]}
				if gotPairs[k] != 1 {
					t.Fatalf("db edge %s->%s appears %d times, want exactly 1", e[0], e[1], gotPairs[k])
				}
			}
			if err := Verify(st, p); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

// TestPresetWithoutWorkflows pins v1 compatibility: presets without the
// optional workflows field (and null / empty-array variants) parse, import,
// and verify exactly as before — no workflow is created.
func TestPresetWithoutWorkflows(t *testing.T) {
	const dept = `{"name": "A", "leader": "a1", "employees": [{"name": "a1", "role": "r"}, {"name": "a2", "role": "r"}]}`
	for _, tc := range []struct{ name, extra string }{
		{"no workflows field", ""},
		{"workflows null", `, "workflows": null`},
		{"workflows empty array", `, "workflows": []`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := `{"name": "old v1", "departments": [` + dept + `]` + tc.extra + `}`
			p := mustParse(t, doc)
			if len(p.Workflows) != 0 {
				t.Fatalf("workflows: %d, want 0", len(p.Workflows))
			}
			st, dbPath := freshStore(t)
			rep, err := Import(st, p, dbPath)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if len(rep.Workflows) != 0 {
				t.Fatalf("report workflows: %d, want 0", len(rep.Workflows))
			}
			wfs, err := st.ListWorkflows()
			if err != nil {
				t.Fatal(err)
			}
			if len(wfs) != 0 {
				t.Fatalf("db workflows: %d, want 0", len(wfs))
			}
			if err := Verify(st, p); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

// TestWorkflowValidation is a table of workflow documents that must be
// rejected by Parse/Validate, each with the locator substring the error
// message must carry.
func TestWorkflowValidation(t *testing.T) {
	const base = `{"name": "x", "departments": [{"name": "A", "leader": "a1", "employees": [` +
		`{"name": "a1", "role": "r"}, {"name": "a2", "role": "r"}, {"name": "a3", "role": "r"}]}], "workflows": %s}`
	cases := []struct {
		name    string
		wfs     string
		wantSub string
	}{
		{"workflow empty name", `[{"name": "", "nodes": ["a1", "a2"]}]`, "workflows[0] has empty name"},
		{"null workflow entry", `[null]`, "workflows[0] is null"},
		{"duplicate workflow name", `[{"name": "W", "nodes": ["a1", "a2"]}, {"name": "W", "nodes": ["a2", "a3"]}]`, `duplicate workflow name "W"`},
		{"node unknown employee", `[{"name": "W", "nodes": ["a1", "ghost"]}]`, `unknown employee "ghost"`},
		{"duplicate node", `[{"name": "W", "nodes": ["a1", "a1"]}]`, `duplicate node "a1"`},
		{"fewer than two nodes", `[{"name": "W", "nodes": ["a1"]}]`, "needs at least 2 nodes"},
		{"edge not a pair", `[{"name": "W", "nodes": ["a1", "a2"], "edges": [["a1"]]}]`, "must be a [from, to] pair"},
		{"edge three elements", `[{"name": "W", "nodes": ["a1", "a2"], "edges": [["a1", "a2", "a3"]]}]`, "must be a [from, to] pair"},
		{"edge wrong json type", `[{"name": "W", "nodes": ["a1", "a2"], "edges": "nope"}]`, "invalid JSON"},
		{"edge references non-node employee", `[{"name": "W", "nodes": ["a1", "a2"], "edges": [["a1", "a3"]]}]`, `edge "a1"->"a3" references an employee that is not a workflow node`},
		{"edge self-loop", `[{"name": "W", "nodes": ["a1", "a2"], "edges": [["a1", "a1"]]}]`, "self-loop edge"},
		{"duplicate edge", `[{"name": "W", "nodes": ["a1", "a2"], "edges": [["a1", "a2"], ["a1", "a2"]]}]`, `duplicate edge "a1"->"a2"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(fmt.Sprintf(base, tc.wfs)))
			if err == nil {
				t.Fatalf("Parse accepted %s document", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
	t.Run("valid workflow passes", func(t *testing.T) {
		doc := fmt.Sprintf(base, `[{"name": "W", "nodes": ["a1", "a2"], "edges": [["a1", "a2"], ["a2", "a1"]]}]`)
		p := mustParse(t, doc)
		if len(p.Workflows) != 1 || len(p.Workflows[0].Nodes) != 2 || len(p.Workflows[0].Edges) != 2 {
			t.Fatalf("unexpected workflows: %+v", p.Workflows)
		}
	})
}

// TestWorkflowVerifyDetectsTamper imports a workflow preset, removes one
// edge behind the preset's back, and asserts Verify fails while naming the
// workflow (the S2 smoke-style tamper check).
func TestWorkflowVerifyDetectsTamper(t *testing.T) {
	doc := `{
	  "name": "x",
	  "departments": [{"name": "A", "leader": "a1", "employees": [{"name": "a1", "role": "r"}, {"name": "a2", "role": "r"}]}],
	  "workflows": [{"name": "Collab", "nodes": ["a1", "a2"], "edges": [["a1", "a2"], ["a2", "a1"]]}]
	}`
	st, dbPath := freshStore(t)
	p := mustParse(t, doc)
	if _, err := Import(st, p, dbPath); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if err := Verify(st, p); err != nil {
		t.Fatalf("Verify before tamper: %v", err)
	}
	wfs, err := st.ListWorkflows()
	if err != nil {
		t.Fatal(err)
	}
	edges, err := st.ListWorkflowEdges(wfs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("edges: %d, want 2", len(edges))
	}
	if err := st.RemoveWorkflowEdge(edges[0].ID); err != nil {
		t.Fatal(err)
	}
	err = Verify(st, p)
	if err == nil {
		t.Fatal("Verify passed after edge removal, want failure")
	}
	if !strings.Contains(err.Error(), "Collab") {
		t.Fatalf("Verify error does not name the workflow: %v", err)
	}
}

// TestImportWorkflowNameConflictZeroPollution guards the reviewer M-1 fix:
// workflow names are unique per database, and importing a second org that
// reuses a workflow name ("Leadership Collab") must fail in the up-front
// name pre-check — before a single department or employee of the second
// org is written (S2 without the fix failed only after the whole roster).
// The error names the workflow and points at a fresh DB.
func TestImportWorkflowNameConflictZeroPollution(t *testing.T) {
	st, dbPath := freshStore(t)
	sw, _ := Get("software")
	pSW, err := Parse(sw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(st, pSW, dbPath); err != nil {
		t.Fatalf("import software: %v", err)
	}
	nEmpBefore := countEmployees(t, st)

	fi, _ := Get("finance")
	pFI, err := Parse(fi)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Import(st, pFI, dbPath)
	if err == nil {
		t.Fatal("import finance into software db succeeded, want workflow-name conflict error")
	}
	if !strings.Contains(err.Error(), "Leadership Collab") || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("conflict error does not name the workflow: %v", err)
	}
	if got := countEmployees(t, st); got != nEmpBefore {
		t.Fatalf("conflicting import polluted the db: employees %d -> %d, want unchanged", nEmpBefore, got)
	}
	depts, err := st.ListDepartments()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range depts {
		if d.Name == "Investment Research Dept" {
			t.Fatal("finance department was created despite the zero-pollution guarantee")
		}
	}
}

// countEmployees is the employee-count probe shared by the pollution tests.
func countEmployees(t *testing.T, st *store.Store) int {
	t.Helper()
	emps, err := st.ListEmployees()
	if err != nil {
		t.Fatal(err)
	}
	return len(emps)
}
