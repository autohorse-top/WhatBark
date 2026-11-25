package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"whatbark/internal/api"
	"whatbark/internal/flow"
	"whatbark/internal/hub"
	"whatbark/internal/store"
)

const adminTok = "admin-test-token"

type env struct {
	srv *httptest.Server
	st  *store.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/agent-test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	h := hub.New()
	fl := flow.New(st, nil, h.Broadcast)
	s := api.New(st, fl, h, adminTok)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = st.Close()
	})
	return &env{srv: srv, st: st}
}

// makeEmployee creates an employee with a known bearer token.
func makeEmployee(t *testing.T, e *env, name, kind string) (int, string) {
	t.Helper()
	dept, err := e.st.CreateDepartment("D-"+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.st.CreateEmployee(name, "tester", kind, dept)
	if err != nil {
		t.Fatalf("CreateEmployee %s: %v", name, err)
	}
	tok := "tok-" + name
	sum := sha256.Sum256([]byte(tok))
	if err := e.st.SetTokenHash(id, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("SetTokenHash %s: %v", name, err)
	}
	return id, tok
}

func req(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	}
	r, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(buf) > 0 {
		if err := json.Unmarshal(buf, &out); err != nil {
			t.Fatalf("unmarshal %s: %v (body=%s)", url, err, buf)
		}
	}
	return resp.StatusCode, out
}

// dispatch asks the boss to send a dispatch task to empID; returns the task id.
func dispatch(t *testing.T, e *env, empID int, body string) int {
	t.Helper()
	code, got := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": empID, "kind": "dispatch", "body": body})
	if code != http.StatusCreated {
		t.Fatalf("dispatch: %d %v", code, got)
	}
	return int(got["task_id"].(float64))
}

// runAgent starts the agent in a goroutine; cleanup cancels and waits.
func runAgent(t *testing.T, e *env, tok string, mockSleep time.Duration) *Agent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ag := New(Config{
		ServerURL:    e.srv.URL,
		Token:        tok,
		PollInterval: 30 * time.Millisecond,
		Heartbeat:    10 * time.Second,
		CancelPoll:   50 * time.Millisecond,
		BaseDir:      t.TempDir(),
		MockSleep:    mockSleep,
		// Hermetic: never auto-detect a real local tool in tests.
		DetectLocalTool: func() string { return "" },
	})
	errCh := make(chan error, 1)
	go func() { errCh <- ag.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("agent Run returned error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("agent did not stop within 10s of cancel")
		}
	})
	return ag
}

// waitTaskStatus polls the store until the task reaches want or the deadline.
func waitTaskStatus(t *testing.T, st *store.Store, taskID int, want string, timeout time.Duration) store.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		tk, err := st.GetTask(taskID)
		if err != nil {
			t.Fatalf("GetTask %d: %v", taskID, err)
		}
		if tk.Status == want {
			return tk
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %d stuck in %s, want %s", taskID, tk.Status, want)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// TestAgentRunsQueuedTask covers the happy path: attach, poll, run mock tool,
// finish with output; last_seen_at persisted on attach.
func TestAgentRunsQueuedTask(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	taskID := dispatch(t, e, empID, "write a haiku")

	runAgent(t, e, tok, 120*time.Millisecond)

	tk := waitTaskStatus(t, e.st, taskID, store.TaskDone, 8*time.Second)
	if !strings.Contains(tk.Output, "mock: done") {
		t.Errorf("task output = %q, want mock lines", tk.Output)
	}
	emp, err := e.st.GetEmployee(empID)
	if err != nil {
		t.Fatal(err)
	}
	if emp.LastSeenAt == nil {
		t.Error("attach did not persist last_seen_at")
	}
}

// TestAgentCancelKillsProcess covers A5: a server-side cancel must kill the
// local tool process (verified via PID liveness probe).
func TestAgentCancelKillsProcess(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	// The bootstrap header is natural language, not shell; deliver the full
	// prompt as $1 and run only the task body (after the "---" line) as a
	// command so "sleep 30" controls the process lifetime.
	setShTool(t, e, empID, `body=$(printf '%s\n' "$1" | awk 'f{print} /^---$/{f=1}')
exec sh -c "$body"`)
	taskID := dispatch(t, e, empID, "sleep 30")

	ag := runAgent(t, e, tok, 100*time.Millisecond)

	var pid int
	deadline := time.Now().Add(8 * time.Second)
	for {
		pid = ag.ActivePID()
		if pid > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent never started a tool process")
		}
		time.Sleep(20 * time.Millisecond)
	}

	code, body := req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/cancel", e.srv.URL, taskID), adminTok,
		map[string]any{"reason": "stop"})
	if code != http.StatusOK {
		t.Fatalf("cancel: %d %v", code, body)
	}

	deadline = time.Now().Add(8 * time.Second)
	for {
		p, err := os.FindProcess(pid)
		if err != nil {
			break
		}
		if err := p.Signal(syscall.Signal(0)); err != nil {
			break // process is gone
		}
		if time.Now().After(deadline) {
			t.Fatal("tool process still alive 8s after cancel")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if tk := waitTaskStatus(t, e.st, taskID, store.TaskCanceled, 2*time.Second); tk.Status != store.TaskCanceled {
		t.Errorf("task status = %s, want canceled", tk.Status)
	}
}

// TestAgentSerialTasks covers A6: two queued tasks run strictly serially
// (task1 finished at or before task2 started).
func TestAgentSerialTasks(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	t1 := dispatch(t, e, empID, "task one")
	t2 := dispatch(t, e, empID, "task two")

	runAgent(t, e, tok, 400*time.Millisecond)

	a := waitTaskStatus(t, e.st, t1, store.TaskDone, 10*time.Second)
	b := waitTaskStatus(t, e.st, t2, store.TaskDone, 10*time.Second)
	if a.FinishedAt == nil || b.StartedAt == nil {
		t.Fatalf("missing timestamps: t1=%+v t2=%+v", a, b)
	}
	if *a.FinishedAt > *b.StartedAt {
		t.Errorf("A6 overlap: task %d finished at %s but task %d started at %s",
			t1, *a.FinishedAt, t2, *b.StartedAt)
	}
}
