// Package preset turns org-structure descriptions (department trees +
// employee rosters, optionally with workflow collaboration graphs) into a
// live whatbark organization: importing a preset creates the departments
// (with their S31 channels), the employees, the department leaders, any
// declared workflow graphs, and issues one bearer token per employee
// (SHA-256 digest stored in the db, plaintext written once to
// <dbPath>-tokens.json).
//
// Presets ship as JSON files embedded in the binary (presets/*.json); any
// file following the same schema is importable via `whatbark preset import
// -file`.
package preset

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"whatbark/internal/org"
	"whatbark/internal/store"
)

// SchemaV1 is the only supported preset schema marker.
const SchemaV1 = "whatbark-preset/v1"

// employeeNameRe mirrors store.nameRe (S7-Minor8): employee names are used
// to build workdir paths, so only safe fragments are allowed.
var employeeNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Employee is one roster entry inside a preset department.
type Employee struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// Kind is "ai" or "human"; empty defaults to "ai".
	Kind string `json:"kind,omitempty"`
}

// Department is one node of the preset org tree. Parent (by name) may only
// reference a department declared EARLIER in the preset, which makes the
// import order a topological order and cycles impossible.
type Department struct {
	Name      string     `json:"name"`
	Parent    string     `json:"parent,omitempty"`
	Leader    string     `json:"leader"`
	Employees []Employee `json:"employees"`
}

// WorkflowSpec is one collaboration graph declared by a preset: a named
// workflow whose nodes are employees (referenced by their preset-wide
// unique name) and whose edges are directed [from, to] pairs of node
// names — e.g. the leadership collab graph wiring every department
// leader to every other leader.
type WorkflowSpec struct {
	Name  string   `json:"name"`
	Nodes []string `json:"nodes"`
	// Edges are [from, to] pairs. The element type is []string, not
	// [2]string: encoding/json silently zero-pads or truncates fixed
	// arrays on length mismatch, so Validate checks len(e)==2 to turn
	// malformed pairs into a locatable error.
	Edges [][]string `json:"edges"`
}

// Preset is the top-level org description.
type Preset struct {
	// Schema is the format marker; empty is accepted as V1, any other
	// value is rejected.
	Schema      string       `json:"schema,omitempty"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Departments []Department `json:"departments"`
	// Workflows are optional collaboration graphs, imported after the
	// org structure itself. Absent/empty means none: v1 presets
	// without the field or with an empty array behave exactly as
	// before (parsing stays lenient under the same schema marker).
	Workflows []*WorkflowSpec `json:"workflows,omitempty"`
}

// Parse decodes and validates a preset document.
func Parse(data []byte) (*Preset, error) {
	var p Preset
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("preset: invalid JSON: %w", err)
	}
	if p.Schema != "" && p.Schema != SchemaV1 {
		return nil, fmt.Errorf("preset: unsupported schema %q (want %q)", p.Schema, SchemaV1)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the preset document. It must pass before any database
// write is attempted.
func (p *Preset) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("preset: name must not be empty")
	}
	if len(p.Departments) == 0 {
		return fmt.Errorf("preset %q: no departments", p.Name)
	}
	seenDept := map[string]bool{}
	seenEmp := map[string]bool{}
	for i, d := range p.Departments {
		if strings.TrimSpace(d.Name) == "" {
			return fmt.Errorf("preset %q: department[%d] has empty name", p.Name, i)
		}
		if seenDept[d.Name] {
			return fmt.Errorf("preset %q: duplicate department %q", p.Name, d.Name)
		}
		seenDept[d.Name] = true
		if d.Parent != "" && !seenDept[d.Parent] {
			return fmt.Errorf("preset %q: department %q references undeclared parent %q (parents must be declared first)", p.Name, d.Name, d.Parent)
		}
		if len(d.Employees) == 0 {
			return fmt.Errorf("preset %q: department %q has no employees", p.Name, d.Name)
		}
		leaderSeen := false
		for _, e := range d.Employees {
			if !employeeNameRe.MatchString(e.Name) {
				return fmt.Errorf("preset %q: employee name %q must match [A-Za-z0-9_-]{1,64}", p.Name, e.Name)
			}
			if strings.TrimSpace(e.Role) == "" {
				return fmt.Errorf("preset %q: employee %s has empty role", p.Name, e.Name)
			}
			if e.Kind != "" && e.Kind != store.KindAI && e.Kind != store.KindHuman {
				return fmt.Errorf("preset %q: employee %s: unsupported kind %q (want ai|human)", p.Name, e.Name, e.Kind)
			}
			if seenEmp[e.Name] {
				return fmt.Errorf("preset %q: duplicate employee name %q (names are global across departments)", p.Name, e.Name)
			}
			seenEmp[e.Name] = true
			if e.Name == d.Leader {
				leaderSeen = true
			}
		}
		if !leaderSeen {
			return fmt.Errorf("preset %q: department %q: leader %q is not among its employees", p.Name, d.Name, d.Leader)
		}
	}
	return p.validateWorkflows(seenEmp)
}

// validateWorkflows checks the optional workflow graphs against the
// globally-unique employee names collected by Validate: workflow names
// must be non-empty and unique per preset, nodes must reference existing
// employees without repetition, and every edge must be a two-element
// [from, to] pair inside the node set with no self-loops or duplicates.
// Absent or empty Workflows passes unchanged (v1 compatibility).
func (p *Preset) validateWorkflows(empNames map[string]bool) error {
	seenWF := map[string]bool{}
	for i, wf := range p.Workflows {
		if wf == nil {
			return fmt.Errorf("preset %q: workflows[%d] is null", p.Name, i)
		}
		if strings.TrimSpace(wf.Name) == "" {
			return fmt.Errorf("preset %q: workflows[%d] has empty name", p.Name, i)
		}
		if seenWF[wf.Name] {
			return fmt.Errorf("preset %q: duplicate workflow name %q", p.Name, wf.Name)
		}
		seenWF[wf.Name] = true
		nodeSeen := map[string]bool{}
		for _, n := range wf.Nodes {
			if !empNames[n] {
				return fmt.Errorf("preset %q: workflow %q references unknown employee %q", p.Name, wf.Name, n)
			}
			if nodeSeen[n] {
				return fmt.Errorf("preset %q: workflow %q: duplicate node %q", p.Name, wf.Name, n)
			}
			nodeSeen[n] = true
		}
		if len(nodeSeen) < 2 {
			return fmt.Errorf("preset %q: workflow %q: needs at least 2 nodes, got %d", p.Name, wf.Name, len(nodeSeen))
		}
		edgeSeen := map[[2]string]bool{}
		for j, e := range wf.Edges {
			if len(e) != 2 {
				return fmt.Errorf("preset %q: workflow %q: edge %d must be a [from, to] pair of employee names, got %d elements", p.Name, wf.Name, j, len(e))
			}
			if e[0] == e[1] {
				return fmt.Errorf("preset %q: workflow %q: self-loop edge %q->%q", p.Name, wf.Name, e[0], e[1])
			}
			if !nodeSeen[e[0]] || !nodeSeen[e[1]] {
				return fmt.Errorf("preset %q: workflow %q: edge %q->%q references an employee that is not a workflow node", p.Name, wf.Name, e[0], e[1])
			}
			key := [2]string{e[0], e[1]}
			if edgeSeen[key] {
				return fmt.Errorf("preset %q: workflow %q: duplicate edge %q->%q", p.Name, wf.Name, e[0], e[1])
			}
			edgeSeen[key] = true
		}
	}
	return nil
}

// Report summarizes a completed import.
type Report struct {
	Preset      string
	Departments []DeptResult
	Employees   int
	TokensFile  string
	// Workflows maps imported workflow name -> workflow id (empty when
	// the preset declares no workflows). For CLI display and tests.
	Workflows map[string]int
	// Tokens maps employee name -> freshly issued plaintext token
	// (only employees that did not already have a stored hash).
	Tokens map[string]string
}

// DeptResult is one created department.
type DeptResult struct {
	Name     string
	ID       int
	Leader   string
	LeaderID int
	Count    int
}

// Import creates the preset organization in st and issues tokens for every
// created employee. Validation (Parse/Validate) must have passed already.
// After the departments, employees, and leaders, any declared workflow
// graphs are created via store.CreateWFlowGraph. The import is
// pre-validated but not a single transaction: on mid-way failure the error
// names the failing department/employee/workflow, and a fresh database
// should be used for a clean retry (documented in README). Two name classes
// are pre-checked before anything is written, so a doomed import fails at
// zero pollution instead of half-way through: top-level department names
// (the org-identity conflict — re-importing the same preset reports the
// department first, matching the historical error surface) and workflow
// names (globally unique — cross-organization imports into the same DB
// collide here since their departments/employees do not).
func Import(st *store.Store, p *Preset, dbPath string) (*Report, error) {
	if err := checkImportConflicts(st, p); err != nil {
		return nil, err
	}
	deptIDs := map[string]int{}
	rep := &Report{Preset: p.Name, Tokens: map[string]string{}, Workflows: map[string]int{}}

	for _, d := range p.Departments {
		var parentID *int
		if d.Parent != "" {
			id := deptIDs[d.Parent]
			parentID = &id
		}
		deptID, _, err := st.CreateDepartmentWithChannel(d.Name, parentID)
		if err != nil {
			return nil, fmt.Errorf("preset: department %q: %w", d.Name, err)
		}
		deptIDs[d.Name] = deptID
		leaderID := 0
		for _, e := range d.Employees {
			kind := e.Kind
			if kind == "" {
				kind = store.KindAI
			}
			id, err := st.CreateEmployee(e.Name, e.Role, kind, deptID)
			if err != nil {
				return nil, fmt.Errorf("preset: department %q: employee %s: %w", d.Name, e.Name, err)
			}
			rep.Employees++
			if e.Name == d.Leader {
				leaderID = id
			}
		}
		if err := st.SetDepartmentLeader(deptID, leaderID); err != nil {
			return nil, fmt.Errorf("preset: department %q: leader %s: %w", d.Name, d.Leader, err)
		}
		rep.Departments = append(rep.Departments, DeptResult{
			Name: d.Name, ID: deptID, Leader: d.Leader, LeaderID: leaderID, Count: len(d.Employees),
		})
	}

	if len(p.Workflows) > 0 {
		wfIDs, err := importWorkflows(st, p)
		if err != nil {
			return nil, err
		}
		rep.Workflows = wfIDs
	}

	file, err := EnsureTokens(st, dbPath)
	if err != nil {
		return nil, fmt.Errorf("preset: tokens: %w", err)
	}
	rep.TokensFile = file
	if data, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(data, &rep.Tokens)
	}
	return rep, nil
}

// checkImportConflicts pre-checks names that would make the import fail
// before writing anything: top-level department names (same-level
// uniqueness, matching the store's own check) and workflow names (globally
// unique per database). Pure read, no writes.
func checkImportConflicts(st *store.Store, p *Preset) error {
	depts, err := st.ListDepartments()
	if err != nil {
		return fmt.Errorf("preset: departments: %w", err)
	}
	topNames := make(map[string]bool, len(depts))
	for _, d := range depts {
		if d.ParentID == nil || *d.ParentID == 0 {
			topNames[d.Name] = true
		}
	}
	for _, d := range p.Departments {
		if d.Parent == "" && topNames[d.Name] {
			return fmt.Errorf("preset: department %q: a department with this name already exists at this level (import into a fresh DB for a clean retry)", d.Name)
		}
	}
	if len(p.Workflows) == 0 {
		return nil
	}
	existing, err := st.ListWorkflows()
	if err != nil {
		return fmt.Errorf("preset: workflows: %w", err)
	}
	seen := make(map[string]bool, len(existing))
	for _, wf := range existing {
		seen[wf.Name] = true
	}
	for _, wf := range p.Workflows {
		if seen[wf.Name] {
			return fmt.Errorf("preset: workflow %q: a workflow with this name already exists in this database (workflow names are unique per database; import into a fresh DB)", wf.Name)
		}
	}
	return nil
}

// importWorkflows creates the preset's workflow graphs once the org
// structure exists: employee names resolve to fresh db ids, node i is
// placed at x=80+i*160, y=200 (horizontal row), and each graph is built
// by store.CreateWFlowGraph in one transaction. A failure is wrapped
// with the workflow name; as documented on Import, the caller should
// retry on a fresh database.
func importWorkflows(st *store.Store, p *Preset) (map[string]int, error) {
	emps, err := st.ListEmployees()
	if err != nil {
		return nil, fmt.Errorf("preset: workflows: %w", err)
	}
	idByName := make(map[string]int, len(emps))
	for _, e := range emps {
		idByName[e.Name] = e.ID
	}
	out := make(map[string]int, len(p.Workflows))
	for _, wf := range p.Workflows {
		g := store.WFlowGraphInput{Name: wf.Name}
		for i, n := range wf.Nodes {
			g.Nodes = append(g.Nodes, store.WFlowNodeInput{
				EmpID: idByName[n],
				X:     float64(80 + i*160),
				Y:     200,
			})
		}
		for _, e := range wf.Edges {
			g.Edges = append(g.Edges, store.WFlowEdgeInput{
				FromEmpID: idByName[e[0]],
				ToEmpID:   idByName[e[1]],
			})
		}
		id, err := st.CreateWFlowGraph(g)
		if err != nil {
			return nil, fmt.Errorf("preset: workflow %q: %w", wf.Name, err)
		}
		out[wf.Name] = id
	}
	return out, nil
}

// EnsureTokens issues a bearer token for every employee whose token_hash is
// empty, stores the SHA-256 digest in the db, and merges the plaintexts
// into <dbPath>-tokens.json (mode 0600). Already-tokened employees are left
// untouched — a lost plaintext is unrecoverable by design, matching the
// one-time-token semantics of POST /api/employees. It returns the file path.
func EnsureTokens(st *store.Store, dbPath string) (string, error) {
	tokensFile := strings.TrimSuffix(dbPath, ".db") + "-tokens.json"
	tokens := map[string]string{}
	if data, err := os.ReadFile(tokensFile); err == nil {
		_ = json.Unmarshal(data, &tokens)
	}

	emps, err := st.ListEmployees()
	if err != nil {
		return "", err
	}
	for _, e := range emps {
		if e.TokenHash != "" {
			continue
		}
		plain, err := randomToken()
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(plain))
		if err := st.SetTokenHash(e.ID, hex.EncodeToString(sum[:])); err != nil {
			return "", err
		}
		tokens[e.Name] = plain
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(tokensFile, data, 0o600); err != nil {
		return "", err
	}
	return tokensFile, nil
}

// randomToken returns a fresh 64-hex-char bearer token (32 random bytes).
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Verify re-reads the database and asserts the imported topology:
// every preset department exists with a channel and its leader, every
// preset employee exists, and every non-leader's reporting chain is exactly
// [its department leader, its department's parent chain...]. Leaders of
// top-level departments have an empty chain (they report to boss 0). For
// presets declaring workflow graphs, each workflow must exist under its
// preset name with exactly the declared node employees and directed edges.
func Verify(st *store.Store, p *Preset) error {
	depts, err := st.ListDepartments()
	if err != nil {
		return err
	}
	emps, err := st.ListEmployees()
	if err != nil {
		return err
	}
	deptByName := map[string]store.Department{}
	for _, d := range depts {
		deptByName[d.Name] = d
	}
	empIDByName := map[string]int{}
	for _, e := range emps {
		empIDByName[e.Name] = e.ID
	}

	ds := make([]org.Department, 0, len(depts))
	for _, d := range depts {
		ds = append(ds, org.Department{ID: d.ID, ParentID: d.ParentID, LeaderID: d.LeaderID})
	}
	es := make([]org.EmployeeRef, 0, len(emps))
	for _, e := range emps {
		es = append(es, org.EmployeeRef{ID: e.ID, DepartmentID: e.DepartmentID})
	}
	snap := org.NewSnapshot(ds, es)

	for _, d := range p.Departments {
		got, ok := deptByName[d.Name]
		if !ok {
			return fmt.Errorf("preset verify: department %q missing", d.Name)
		}
		if got.GroupID == nil {
			return fmt.Errorf("preset verify: department %q has no channel", d.Name)
		}
		if got.LeaderID == nil {
			return fmt.Errorf("preset verify: department %q has no leader", d.Name)
		}
		wantLeader := empIDByName[d.Leader]
		if *got.LeaderID != wantLeader {
			return fmt.Errorf("preset verify: department %q: leader mismatch (got %d, want %d)", d.Name, *got.LeaderID, wantLeader)
		}
	}
	for _, d := range p.Departments {
		for _, e := range d.Employees {
			want := expectedChain(p, d.Name, e.Name, empIDByName)
			gotChain, err := org.ReportingChain(snap, empIDByName[e.Name])
			if err != nil {
				return fmt.Errorf("preset verify: %s: %w", e.Name, err)
			}
			if len(gotChain) != len(want) {
				return fmt.Errorf("preset verify: %s: chain length %d, want %d", e.Name, len(gotChain), len(want))
			}
			for i := range want {
				if gotChain[i].EmpID != want[i] {
					return fmt.Errorf("preset verify: %s: chain %v, want %v", e.Name, gotChain, want)
				}
			}
		}
	}

	if len(p.Workflows) > 0 {
		wfs, err := st.ListWorkflows()
		if err != nil {
			return err
		}
		wfByName := make(map[string]store.Workflow, len(wfs))
		for _, w := range wfs {
			wfByName[w.Name] = w
		}
		for _, spec := range p.Workflows {
			if err := verifyWorkflow(st, wfByName, empIDByName, spec); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyWorkflow asserts one preset workflow against the database: the
// workflow exists under the preset name, its node employee ids are exactly
// the spec's node names (as a set), and its directed edges are exactly the
// spec's [from, to] pairs — so missing nodes, extra nodes, missing edges,
// or rewired edges all fail with a message naming the workflow.
func verifyWorkflow(st *store.Store, wfByName map[string]store.Workflow, empIDByName map[string]int, spec *WorkflowSpec) error {
	wf, ok := wfByName[spec.Name]
	if !ok {
		return fmt.Errorf("preset verify: workflow %q missing", spec.Name)
	}
	nodes, err := st.ListWorkflowNodes(wf.ID)
	if err != nil {
		return fmt.Errorf("preset verify: workflow %q: %w", spec.Name, err)
	}
	empIDByNodeID := make(map[int]int, len(nodes))
	gotEmps := make(map[int]bool, len(nodes))
	for _, n := range nodes {
		empIDByNodeID[n.ID] = n.EmpID
		gotEmps[n.EmpID] = true
	}
	for _, name := range spec.Nodes {
		id := empIDByName[name]
		if !gotEmps[id] {
			return fmt.Errorf("preset verify: workflow %q: node employee %s (id %d) missing", spec.Name, name, id)
		}
	}
	if len(nodes) != len(spec.Nodes) {
		return fmt.Errorf("preset verify: workflow %q: %d nodes, want %d", spec.Name, len(nodes), len(spec.Nodes))
	}
	edges, err := st.ListWorkflowEdges(wf.ID)
	if err != nil {
		return fmt.Errorf("preset verify: workflow %q: %w", spec.Name, err)
	}
	gotEdges := make(map[[2]int]bool, len(edges))
	for _, e := range edges {
		gotEdges[[2]int{empIDByNodeID[e.FromNodeID], empIDByNodeID[e.ToNodeID]}] = true
	}
	for _, pair := range spec.Edges {
		key := [2]int{empIDByName[pair[0]], empIDByName[pair[1]]}
		if !gotEdges[key] {
			return fmt.Errorf("preset verify: workflow %q: edge %s->%s missing", spec.Name, pair[0], pair[1])
		}
	}
	if len(edges) != len(spec.Edges) {
		return fmt.Errorf("preset verify: workflow %q: %d edges, want %d", spec.Name, len(edges), len(spec.Edges))
	}
	return nil
}

// expectedChain computes the expected reporting chain for employee emp of
// department deptName from the preset structure alone: a member reports to
// its own department leader; a department leader reports to the parent
// department's leader, and so on up the tree. Names resolve to db ids via
// empIDByName (the database the preset was imported into).
func expectedChain(p *Preset, deptName, emp string, empIDByName map[string]int) []int {
	var chain []int
	if emp != findDept(p, deptName).Leader {
		chain = append(chain, empIDByName[findDept(p, deptName).Leader])
	}
	dept := findDept(p, deptName)
	for dept.Parent != "" {
		chain = append(chain, empIDByName[findDept(p, dept.Parent).Leader])
		dept = findDept(p, dept.Parent)
	}
	return chain
}

func findDept(p *Preset, name string) Department {
	for _, d := range p.Departments {
		if d.Name == name {
			return d
		}
	}
	return Department{}
}
