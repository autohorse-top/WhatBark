package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"whatbark/internal/store"
	"whatbark/internal/tool"
)

// V-1: per-employee tool session — reuse without re-init, idle reclaim,
// failure reclaim, cancel never reclaims, one-shot CLI sessions.

func TestSessionMockReuseNoReinit(t *testing.T) {
	s := NewSession(tool.Spec{Name: "mock"}, time.Minute, 3)

	g1, h1 := s.Begin("task one")
	if g1 != 1 {
		t.Fatalf("first task gen = %d, want 1", g1)
	}
	if want := "session: gen=1 turn=1 remembered=0"; !strings.Contains(h1, want) {
		t.Errorf("first header = %q, want %q", h1, want)
	}
	s.Finish(false, false)

	g2, h2 := s.Begin("task two")
	if g2 != 1 {
		t.Fatalf("second task gen = %d, want 1 (must reuse the session, not re-init)", g2)
	}
	for _, want := range []string{"turn=2", "remembered=1", `"task one"`} {
		if !strings.Contains(h2, want) {
			t.Errorf("second header = %q, want %q", h2, want)
		}
	}
	s.Finish(false, false)

	if got := s.InitCount(); got != 1 {
		t.Errorf("InitCount = %d, want 1 (no re-init across consecutive tasks)", got)
	}
}

func TestSessionIdleReclaim(t *testing.T) {
	s := NewSession(tool.Spec{Name: "mock"}, 30*time.Millisecond, 0)
	s.Begin("a")
	s.Finish(false, false)
	time.Sleep(80 * time.Millisecond)

	if !s.Reap() {
		t.Fatal("Reap did not reclaim the idle session")
	}
	g, h := s.Begin("b")
	if g != 2 {
		t.Fatalf("post-reclaim gen = %d, want 2 (fresh init)", g)
	}
	if want := "turn=1 remembered=0"; !strings.Contains(h, want) {
		t.Errorf("post-reclaim header = %q, want turn=1 remembered=0", h)
	}
	if got := s.InitCount(); got != 2 {
		t.Errorf("InitCount = %d, want 2", got)
	}
}

func TestSessionFailureReclaim(t *testing.T) {
	s := NewSession(tool.Spec{Name: "mock"}, time.Minute, 2)
	s.Begin("a")
	s.Finish(true, false) // failure 1
	s.Begin("b")
	if s.Generation() != 1 {
		t.Fatal("session reclaimed after a single failure (maxFailures=2)")
	}
	s.Finish(true, false) // failure 2 -> reclaim
	s.Begin("c")
	if got := s.Generation(); got != 2 {
		t.Fatalf("gen after failure reclaim = %d, want 2", got)
	}
}

func TestSessionCancelNeverReclaims(t *testing.T) {
	s := NewSession(tool.Spec{Name: "mock"}, time.Minute, 1)
	s.Begin("a")
	s.Finish(false, true) // canceled, not a failure
	g, _ := s.Begin("b")
	if g != 1 {
		t.Fatalf("gen = %d after cancel, want 1 (cancel must not reclaim)", g)
	}
}

func TestSessionCLIOneShot(t *testing.T) {
	s := NewSession(tool.Spec{Name: "opencode"}, time.Minute, 3)
	g1, h1 := s.Begin("x")
	if h1 != "" {
		t.Errorf("CLI header = %q, want empty (no injection into tool output)", h1)
	}
	s.Finish(false, false)
	g2, _ := s.Begin("y")
	if g2 != g1+1 {
		t.Errorf("CLI gen = %d, want %d (one-shot: every task re-inits)", g2, g1+1)
	}
}

// TestAgentTwoTasksOneSession (V-1 acceptance 1): two consecutive mock tasks
// share one tool session — the second task's server-stored report carries the
// session header with gen=1 (no re-init) and the first task's body as
// remembered context.
func TestAgentTwoTasksOneSession(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	t1 := dispatch(t, e, empID, "first body marker")
	t2 := dispatch(t, e, empID, "second body marker")

	runAgent(t, e, tok, 120*time.Millisecond)

	a := waitTaskStatus(t, e.st, t1, store.TaskDone, 15*time.Second)
	b := waitTaskStatus(t, e.st, t2, store.TaskDone, 15*time.Second)

	if !strings.Contains(a.Output, "session: gen=1 turn=1 remembered=0") {
		t.Errorf("task 1 output lacks first-turn session header; output=%q", a.Output)
	}
	for _, want := range []string{"session: gen=1 turn=2 remembered=1", `"first body marker"`} {
		if !strings.Contains(b.Output, want) {
			t.Errorf("task 2 output lacks %q; output=%q", want, b.Output)
		}
	}
}

// TestAgentIdleReclaim (V-1): with a short SessionIdle, a session that has
// been idle longer than the window is reclaimed by the poll loop; the next
// task re-inits (new generation).
func TestAgentIdleReclaim(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	t1 := dispatch(t, e, empID, "idle probe one")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag := New(Config{
		ServerURL:          e.srv.URL,
		Token:              tok,
		PollInterval:       30 * time.Millisecond,
		Heartbeat:          10 * time.Second,
		CancelPoll:         50 * time.Millisecond,
		BaseDir:            t.TempDir(),
		MockSleep:          50 * time.Millisecond,
		SessionIdle:        60 * time.Millisecond,
		SessionMaxFailures: 0,
		// Hermetic: never auto-detect a real local tool in tests.
		DetectLocalTool: func() string { return "" },
	})
	errCh := make(chan error, 1)
	go func() { errCh <- ag.Run(ctx) }()

	waitTaskStatus(t, e.st, t1, store.TaskDone, 8*time.Second)

	// The poll loop's reaper must close the idle session (gen -> 0).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ag.SessionGeneration() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session not reclaimed after idle window (gen still live)")
		}
		time.Sleep(20 * time.Millisecond)
	}

	t2 := dispatch(t, e, empID, "idle probe two")
	b := waitTaskStatus(t, e.st, t2, store.TaskDone, 8*time.Second)
	if !strings.Contains(b.Output, "session: gen=2 turn=1") {
		t.Errorf("task 2 output = %q, want gen=2 (re-init after idle reclaim)", b.Output)
	}
}
