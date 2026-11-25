package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"whatbark/internal/store"
)

func TestSPAElements(t *testing.T) {
	e := newEnv(t)
	r, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /: got %d want 200", r.StatusCode)
	}
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		n, readErr := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if readErr != nil {
			break
		}
	}
	body := string(buf)
	want := []string{
		`data-block="org"`,
		`data-block="wflow"`,
		`data-block="projects"`,
		`data-block="status"`,
		`data-block="inbox"`,
		`data-block="sched"`,
		`data-block="chat"`,
		`data-block="settings"`,
	}
	for _, w := range want {
		found := false
		for i := 0; i+len(w) <= len(body); i++ {
			if body[i:i+len(w)] == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SPA missing element %s", w)
		}
	}
	// S20: unknown avatar names must fall back to default.svg (200), not 404.
	for _, p := range []string{"/core.js", "/style.css", "/blocks-wflow.js", "/blocks-inbox.js", "/blocks-chat.js", "/blocks-settings.js", "/avatars/boss.svg", "/avatars/default.svg", "/avatars/no-such-employee-xyz.svg"} {
		res, err := http.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: got %d want 200", p, res.StatusCode)
		}
		if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("GET %s Cache-Control: got %q want no-store", p, got)
		}
	}
}

func TestCORSPreflight(t *testing.T) {
	e := newEnv(t)
	reqObj, _ := http.NewRequest(http.MethodOptions, e.srv.URL+"/api/employees", nil)
	resp, err := http.DefaultClient.Do(reqObj)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS /api/employees: got %d want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin: got %q want *", got)
	}
}

func TestMoveWorkflowNode(t *testing.T) {
	e := newEnv(t)
	deptID, err := e.st.CreateDepartment("MoveDept", nil)
	if err != nil {
		t.Fatal(err)
	}
	empID, empTok := makeEmployee(t, e, "Mover", store.KindAI, deptID)
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/workflows", adminTok, map[string]any{"name": "move-test"})
	if code != http.StatusCreated {
		t.Fatalf("create workflow: got %d body %v", code, body)
	}
	wfID := int(body["id"].(float64))
	code, body = req(t, http.MethodPost, e.srv.URL+"/api/workflows/"+strconv.Itoa(wfID)+"/nodes", adminTok, map[string]any{
		"employee_id": empID, "x": 10.0, "y": 20.0,
	})
	if code != http.StatusCreated {
		t.Fatalf("add node: got %d body %v", code, body)
	}
	nodeID := int(body["id"].(float64))
	code, body = req(t, http.MethodPut, e.srv.URL+"/api/workflows/"+strconv.Itoa(wfID)+"/nodes/"+strconv.Itoa(nodeID), adminTok, map[string]any{"x": 130.5, "y": 240.0})
	if code != http.StatusOK {
		t.Fatalf("move node: got %d body %v", code, body)
	}
	code, body = req(t, http.MethodGet, e.srv.URL+"/api/workflows/"+strconv.Itoa(wfID), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("get workflow: got %d", code)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Nodes []struct {
			ID int     `json:"id"`
			X  float64 `json:"x"`
			Y  float64 `json:"y"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	moved := false
	for _, n := range wf.Nodes {
		if n.ID == nodeID {
			moved = true
			if n.X != 130.5 || n.Y != 240.0 {
				t.Errorf("node position: got (%v,%v) want (130.5,240)", n.X, n.Y)
			}
		}
	}
	if !moved {
		t.Fatalf("moved node not found in workflow nodes")
	}
	code, _ = req(t, http.MethodPut, e.srv.URL+"/api/workflows/"+strconv.Itoa(wfID)+"/nodes/"+strconv.Itoa(nodeID), empTok, map[string]any{"x": 1, "y": 2})
	if code != http.StatusForbidden {
		t.Errorf("non-admin move node: got %d want 403", code)
	}
}
