package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"whatbark/internal/api"
	"whatbark/internal/flow"
	"whatbark/internal/hub"
	"whatbark/internal/store"
)

// TestAgentWatchStopsAfterDone (S7-M1 regression): the cancel-watcher must
// stop probing GET /api/tasks/{id} once the task reaches a terminal state.
// Before the fix the watcher polled forever after done/failed (observed ~12
// extra probes in 600ms at a 50ms poll interval).
func TestAgentWatchStopsAfterDone(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/watch-leak.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	h := hub.New()
	fl := flow.New(st, nil, h.Broadcast)
	s := api.New(st, fl, h, adminTok)

	// Count only the watcher's probes: GET /api/tasks/{id} with no query.
	// The poll loop uses GET /api/tasks?employee=N (path "/api/tasks").
	var probes int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/tasks/") && r.URL.RawQuery == "" {
			atomic.AddInt64(&probes, 1)
		}
		s.Handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	e := &env{srv: srv, st: st}
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	taskID := dispatch(t, e, empID, "watch-leak probe")

	ag := New(Config{
		ServerURL:    srv.URL,
		Token:        tok,
		PollInterval: 30 * time.Millisecond,
		Heartbeat:    10 * time.Second,
		CancelPoll:   50 * time.Millisecond,
		BaseDir:      t.TempDir(),
		MockSleep:    100 * time.Millisecond,
		// Hermetic: never auto-detect a real local tool in tests.
		DetectLocalTool: func() string { return "" },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ag.Run(ctx) }()

	waitTaskStatus(t, st, taskID, store.TaskDone, 8*time.Second)
	base := atomic.LoadInt64(&probes)
	time.Sleep(600 * time.Millisecond)
	extra := atomic.LoadInt64(&probes) - base
	if extra > 2 {
		t.Errorf("watcher kept polling %d times in 600ms after task done (want <=2); S7-M1 leak not fixed", extra)
	}
}

// TestAgentReassignStopsOldOwner (S27 review R1 regression): once a running
// task is reassigned to another employee, the old owner's status probe starts
// returning 403. Pre-fix, watchCancel treated every probe error as transient
// and spun forever — the tool kept executing a task that no longer belongs to
// it (double execution). Post-fix, 403/404 on the probe is treated like a
// server-side preempt: the watcher stops probing and the tool is canceled
// without finish/fail, leaving the task queued for the new owner.
func TestAgentReassignStopsOldOwner(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/reassign-spin.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	h := hub.New()
	fl := flow.New(st, nil, h.Broadcast)
	s := api.New(st, fl, h, adminTok)

	// Count only the watcher's probes: GET /api/tasks/{id} with no query.
	var probes int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/tasks/") && r.URL.RawQuery == "" {
			atomic.AddInt64(&probes, 1)
		}
		s.Handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	e := &env{srv: srv, st: st}
	empA, tokA := makeEmployee(t, e, "RA", store.KindAI)
	empB, _ := makeEmployee(t, e, "RB", store.KindAI)
	// The bootstrap header is natural language, not shell; deliver the full
	// prompt as $1 and run only the task body (after the "---" line) as a
	// command so "sleep 30" controls the process lifetime.
	setShTool(t, e, empA, `body=$(printf '%s\n' "$1" | awk 'f{print} /^---$/{f=1}')
exec sh -c "$body"`)
	taskID := dispatch(t, e, empA, "sleep 30")

	ag := runAgent(t, e, tokA, 100*time.Millisecond)

	// Wait until the old owner's tool process is running.
	var pid int
	deadline := time.Now().Add(8 * time.Second)
	for {
		pid = ag.ActivePID()
		if pid > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent A never started a tool process")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Reassign the running task to empB (admin). The old owner's next probe
	// must get 403 (self-scope) instead of 200.
	code, body := req(t, http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%d/reassign", e.srv.URL, taskID), adminTok,
		map[string]any{"to": empB, "reason": "R1 test"})
	if code != http.StatusOK {
		t.Fatalf("reassign: %d %v", code, body)
	}

	// The old owner's tool process must be canceled (preempt path).
	deadline = time.Now().Add(8 * time.Second)
	for {
		if p, err := os.FindProcess(pid); err == nil {
			if err := p.Signal(syscall.Signal(0)); err == nil {
				if time.Now().After(deadline) {
					t.Fatal("old owner's tool still alive 8s after reassign (R1: tool never canceled)")
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
		}
		break // process is gone
	}

	// The watcher must stop probing: probe count stable for 600ms.
	base := atomic.LoadInt64(&probes)
	time.Sleep(600 * time.Millisecond)
	if extra := atomic.LoadInt64(&probes) - base; extra > 2 {
		t.Errorf("watcher kept probing %d times in 600ms after reassign (want <=2); R1 spin not fixed", extra)
	}

	// The task must remain queued for the new owner: the old agent must not
	// finish/fail it.
	tk, err := st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.EmpID != empB || tk.Status != store.TaskQueued {
		t.Errorf("task after reassign: owner=%d status=%s, want owner=%d status=%s",
			tk.EmpID, tk.Status, empB, store.TaskQueued)
	}
}
