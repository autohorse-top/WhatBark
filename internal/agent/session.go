package agent

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"whatbark/internal/tool"
)

// Session is the per-employee tool session (V-1 persistent session).
//
// Lifetime model:
//   - Begin lazily creates a generation ("init") on the first task and
//     reuses it for consecutive tasks while it stays alive — the second task
//     does NOT re-init (V-1 acceptance 1).
//   - The built-in mock tool keeps cross-task context inside the session
//     (turn counter + previous task bodies); its output header reports
//     gen/turn/remembered/last so reuse is observable in the task report.
//   - External CLI tools (opencode/claudecode) are one-shot per task: their
//     persistent interactive protocols are not verified, so a CLI session
//     re-initializes for every task (gen increments per task) while the same
//     lifecycle/reclaim machinery applies uniformly.
//   - A generation is reclaimed when idle (no task in flight) longer than
//     s.idle since the last task finished, or after s.maxFailures consecutive
//     task failures (s.maxFailures = 0 disables failure reclamation).
//     Cancellation/preemption never count as failures and never reclaim.
//
// S18/S27 server semantics (attach/heartbeat/cancel/preempt) are untouched:
// they live in the agent loop and the server; the session only owns the tool
// invocation lifecycle and its reclaim policy.
type Session struct {
	spec tool.Spec
	idle time.Duration // 0 = never reclaim on idle
	fail int           // consecutive failures that trigger reclaim; 0 = disabled

	mu         sync.Mutex
	gen        int      // live generation (0 = none live)
	initCount  int      // total generations ever created
	turns      int      // tasks begun in the live generation
	ctxHistory []string // task bodies of the live generation (mock only)
	lastUsed   time.Time
	failures   int
	active     bool // a task is currently running in this session
}

// NewSession creates a session; the first Begin performs the init.
func NewSession(spec tool.Spec, idle time.Duration, maxFailures int) *Session {
	return &Session{spec: spec, idle: idle, fail: maxFailures}
}

// Begin ensures a live generation and records a task start. It returns the
// generation number and a header line the agent posts to the task output
// before the tool's own lines ("" for non-mock tools, whose sessions are
// one-shot by design and whose output must stay pure).
func (s *Session) Begin(taskBody string) (gen int, header string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapIdleLocked()
	s.active = true
	if s.isMock() {
		if s.gen == 0 {
			s.initLocked()
		}
		s.turns++
		remembered := len(s.ctxHistory)
		s.ctxHistory = append(s.ctxHistory, taskBody)
		if len(s.ctxHistory) > 64 {
			s.ctxHistory = s.ctxHistory[len(s.ctxHistory)-64:]
		}
		last := ""
		if remembered > 0 {
			last = s.ctxHistory[remembered-1]
		}
		header = fmt.Sprintf("session: gen=%d turn=%d remembered=%d last=%q",
			s.gen, s.turns, remembered, truncateRunes(last, 60))
	} else {
		// One-shot CLI session: every task is a fresh generation.
		s.initLocked()
	}
	return s.gen, header
}

// Finish records a task outcome. canceled=true marks a server-side cancel or
// preemption (S18/S27) — never a session failure, never a reclaim.
func (s *Session) Finish(failed, canceled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
	s.lastUsed = time.Now()
	if canceled {
		return
	}
	if !failed {
		s.failures = 0
		return
	}
	s.failures++
	if s.fail > 0 && s.failures >= s.fail {
		s.resetLocked() // poisoned session: the next Begin re-inits
	}
}

// Reap reclaims the live generation when it has been idle (no task in
// flight) longer than the configured window. It is safe to call from the
// agent's poll loop between tasks. Returns true when a generation was
// reclaimed.
func (s *Session) Reap() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen == 0 {
		return false
	}
	s.reapIdleLocked()
	return s.gen == 0
}

// ToolName returns the tool this session was created for (immutable).
func (s *Session) ToolName() string { return s.spec.Name }

// InitCount returns how many generations this session has ever created
// (observability for tests and logs).
func (s *Session) InitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initCount
}

// Generation returns the live generation number (0 = none live).
func (s *Session) Generation() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// reapIdleLocked reclaims the live generation when idle past s.idle.
// Callers hold mu.
func (s *Session) reapIdleLocked() {
	// A task currently in flight keeps the session alive: the idle window is
	// measured from the last *finished* task, and Finish clears the flag.
	if s.gen == 0 || s.idle <= 0 || s.active {
		return
	}
	if time.Since(s.lastUsed) > s.idle {
		s.resetLocked()
	}
}

// initLocked starts a new generation. Callers hold mu.
func (s *Session) initLocked() {
	s.initCount++
	s.gen = s.initCount
	s.turns = 0
	s.ctxHistory = nil
	s.failures = 0
}

// resetLocked ends the live generation without creating a new one.
// Callers hold mu.
func (s *Session) resetLocked() {
	s.gen = 0
	s.turns = 0
	s.ctxHistory = nil
	s.failures = 0
}

func (s *Session) isMock() bool {
	return strings.EqualFold(strings.TrimSpace(s.spec.Name), "mock")
}

// truncateRunes returns at most n runes of s, replacing the cut tail with an
// ellipsis (rune-safe, unlike the byte-based truncate in exec.go).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
