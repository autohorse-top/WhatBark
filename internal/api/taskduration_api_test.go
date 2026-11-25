package api

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"
	"time"
)

// TestTaskRowDuration (task-duration-artifacts A2): GET /api/tasks rows carry
// server-computed duration/duration_sec; queued rows omit both keys, running
// rows report a live (>=1s) span, done rows are frozen at finish.
func TestTaskRowDuration(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)

	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "work"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: %d %v", code, body)
	}

	rows := func() []map[string]any {
		t.Helper()
		code, body := req(t, http.MethodGet, fmt.Sprintf("%s/api/tasks?employee=%d", e.srv.URL, w1), w1tok, nil)
		if code != http.StatusOK {
			t.Fatalf("list tasks: status %d", code)
		}
		raw, _ := body["tasks"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, m := range raw {
			out = append(out, m.(map[string]any))
		}
		return out
	}

	// Queued: neither key present.
	queued := rows()[0]
	if _, has := queued["duration"]; has {
		t.Fatalf("queued row carries duration: %v", queued)
	}
	if _, has := queued["duration_sec"]; has {
		t.Fatalf("queued row carries duration_sec: %v", queued)
	}

	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, int(body["task_id"].(float64))), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("start: status %d", code)
	}
	time.Sleep(1100 * time.Millisecond)

	// Running: live span, at least 1 second.
	row := rows()[0]
	if sec, _ := row["duration_sec"].(float64); sec < 1 {
		t.Errorf("running duration_sec = %v, want >= 1", row["duration_sec"])
	}
	if label, _ := row["duration"].(string); label == "" {
		t.Error("running row missing duration label")
	}

	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, int(body["task_id"].(float64))), w1tok,
		map[string]any{"output": "done"})
	if code != http.StatusOK {
		t.Fatalf("finish: status %d", code)
	}
	row = rows()[0]
	if label, _ := row["duration"].(string); label == "" {
		t.Errorf("done row missing duration: %v", row)
	}
	if sec, _ := row["duration_sec"].(float64); sec < 1 {
		t.Errorf("done duration_sec = %v, want >= 1", sec)
	}
}

// TestInboxTaskDuration (task-duration-artifacts A): the ?to= (inbox) branch
// of GET /api/messages annotates rows carrying task_id with a
// server-computed task_duration label; the 1:1 ?with= branch does not
// (S28 intent: no task annotations outside group/inbox views). Queued tasks
// carry no duration.
func TestInboxTaskDuration(t *testing.T) {
	e := newEnv(t)
	_, l1tok, w1, w1tok := topo(t, e)

	// L1 dispatches to W1 -> task + dispatch message in W1's inbox.
	code, body := req(t, http.MethodPost, e.srv.URL+"/api/messages", l1tok,
		map[string]any{"to": w1, "kind": "dispatch", "body": "work"})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: %d %v", code, body)
	}
	taskID := int(body["task_id"].(float64))

	findRow := func(tok, url string, wantTaskID int) (map[string]any, bool) {
		t.Helper()
		code, body := req(t, http.MethodGet, url, tok, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s: status %d", url, code)
		}
		for _, m := range body["messages"].([]any) {
			row := m.(map[string]any)
			if row["task_id"] != nil && int(row["task_id"].(float64)) == wantTaskID {
				return row, true
			}
		}
		return nil, false
	}

	// Queued: inbox row must NOT carry task_duration.
	if row, ok := findRow(w1tok, fmt.Sprintf("%s/api/messages?to=%d", e.srv.URL, w1), taskID); !ok {
		t.Fatal("no dispatch row in W1 inbox")
	} else if row["task_duration"] != nil {
		t.Fatalf("queued row carries task_duration: %v", row)
	}

	// Start, wait >1s, finish -> done tasks carry a duration.
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/start", e.srv.URL, taskID), w1tok, nil)
	if code != http.StatusOK {
		t.Fatalf("start: status %d", code)
	}
	time.Sleep(1100 * time.Millisecond)
	code, _ = req(t, http.MethodPost, fmt.Sprintf("%s/api/tasks/%d/finish", e.srv.URL, taskID), w1tok,
		map[string]any{"output": "done"})
	if code != http.StatusOK {
		t.Fatalf("finish: status %d", code)
	}

	row, ok := findRow(w1tok, fmt.Sprintf("%s/api/messages?to=%d", e.srv.URL, w1), taskID)
	if !ok {
		t.Fatal("no dispatch row in W1 inbox after finish")
	}
	re := regexp.MustCompile(`^\d+s$|^\d+m \d+s$|^\d+h \d+m$`)
	if label, _ := row["task_duration"].(string); !re.MatchString(label) {
		t.Errorf("inbox task_duration = %q, want label matching %q", label, re.String())
	}

	// 1:1 ?with= view (L1 looking at the thread with W1): no annotation.
	row, ok = findRow(l1tok, fmt.Sprintf("%s/api/messages?with=%d", e.srv.URL, w1), taskID)
	if !ok {
		t.Fatal("no dispatch row in L1 1:1 view")
	}
	if _, has := row["task_duration"]; has {
		t.Errorf("?with= row carries task_duration: %v", row)
	}
}
