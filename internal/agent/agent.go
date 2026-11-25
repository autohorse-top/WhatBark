// Package agent implements the local executor: polls queued tasks from the
// platform, runs the configured tool, streams output back, and honors
// server-side cancellation.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"whatbark/internal/tool"
)

// Task is the subset of task fields the agent reads from the API.
type Task struct {
	ID       int    `json:"id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
	Input    string `json:"input"`
	// CreatedAt is the server-side creation timestamp (UTC "2006-01-02 15:04:05");
	// used as the mtime reference for collecting task-produced artifacts.
	CreatedAt string `json:"created_at"`
}

// Employee mirrors the /api/me response for a non-admin caller.
type Employee struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Role     string `json:"role"`
	Tool     string `json:"tool"`
	ToolArgs string `json:"tool_args"`
	Workdir  string `json:"workdir"`
}

// Config holds agent runtime settings.
type Config struct {
	ServerURL    string
	Token        string
	PollInterval time.Duration
	Heartbeat    time.Duration
	CancelPoll   time.Duration
	BaseDir      string
	MockSleep    time.Duration
	// SessionIdle reclaims the tool session when it has been idle this long
	// since the last task finished (V-1; default 10m).
	SessionIdle time.Duration
	// SessionMaxFailures reclaims the session after this many consecutive
	// task failures (V-1; default 3, 0 = disabled).
	SessionMaxFailures int
	// LLMMaxRetries (P1a) is the number of retries after a TRANSIENT tool
	// failure (rate limit, 529, overloaded_error, network reset — see
	// isTransientToolError) before the task is failed. Total attempts =
	// 1 + LLMMaxRetries. Zero (the zero-value) takes the default of 2.
	// Start failures (missing binary) and non-transient exits are never
	// retried.
	LLMMaxRetries int
	// LLMRetryBackoff (P1a) is the initial backoff before the first retry;
	// it doubles after each retry, capped at 30s. Zero takes the default
	// of 2s. During the backoff the agent still honors server-side cancel /
	// preempt / shutdown (no retry after those).
	LLMRetryBackoff time.Duration
	// DetectLocalTool, when non-nil, overrides the default tool.DetectLocalTool
	// used to auto-select a real LLM tool when the employee's configured tool
	// is "mock". Tests set this to func() string { return "" } to disable
	// auto-detection and keep hermetic mock behavior.
	DetectLocalTool func() string
}

// Agent is a local executor bound to one employee account.
type Agent struct {
	cfg     Config
	client  *http.Client
	mu      sync.Mutex
	empID   int
	emp     *Employee // authenticated employee (S18: bootstrap prompt, context files)
	spec    tool.Spec
	active  *tool.Process
	session *Session // V-1: per-employee tool session (lazy; guarded by mu)
}

// ActivePID returns the PID of the currently running tool process (0 if none).
func (a *Agent) ActivePID() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		return 0
	}
	return a.active.Pid()
}

// SessionGeneration returns the live tool-session generation (0 = none live,
// i.e. not yet created or already reclaimed). V-1 observability for tests.
func (a *Agent) SessionGeneration() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		return 0
	}
	return a.session.Generation()
}

// reapSession reclaims the tool session when it has been idle past its window
// (V-1). Called from the poll loop between tasks; a task in flight is never
// reclaimed (Session.Reap guards on the active flag).
func (a *Agent) reapSession() {
	a.mu.Lock()
	se := a.session
	a.mu.Unlock()
	if se != nil {
		se.Reap()
	}
}

// New builds an Agent with sensible defaults for zero-value fields.
func New(cfg Config) *Agent {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.CancelPoll <= 0 {
		cfg.CancelPoll = time.Second
	}
	if cfg.BaseDir == "" {
		home, _ := os.UserHomeDir()
		cfg.BaseDir = home
	}
	if cfg.SessionIdle <= 0 {
		cfg.SessionIdle = 10 * time.Minute
	}
	if cfg.SessionMaxFailures < 0 {
		cfg.SessionMaxFailures = 3
	}
	// P1a defaults: retry transient LLM tool failures twice (3 attempts),
	// starting at a 2s backoff. Deterministic tools (mock, sh sleep, ...)
	// never emit transient markers, so this is behavior-neutral for them.
	if cfg.LLMMaxRetries <= 0 {
		cfg.LLMMaxRetries = 2
	}
	if cfg.LLMRetryBackoff <= 0 {
		cfg.LLMRetryBackoff = 2 * time.Second
	}
	return &Agent{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

// Run starts the agent loop. It blocks until ctx is canceled.
// Flow: authenticate (/api/me) -> initial attach (requeues orphaned running
// tasks, S18) -> write context files (S18) -> heartbeat goroutine ->
// poll queued tasks (highest priority first, S18) -> execute one at a time
// (A6 serial guarantee).
func (a *Agent) Run(ctx context.Context) error {
	emp, err := a.getMe(ctx)
	if err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}
	if emp.Kind != "ai" {
		return fmt.Errorf("employee %s is kind=%q, agent requires ai", emp.Name, emp.Kind)
	}
	a.mu.Lock()
	a.empID = emp.ID
	a.emp = emp
	a.spec = a.buildToolSpec(emp)
	a.mu.Unlock()
	slog.Info("agent ready", "emp", emp.ID, "name", emp.Name, "tool", a.spec.Name)

	// S18: the initial attach tells the server this is a fresh agent process
	// so it requeues any running tasks left behind by a crashed predecessor.
	// Heartbeats keep the empty body, which must never requeue.
	if err := a.do(ctx, "POST", "/api/agent/attach", map[string]any{"initial": true}, nil); err != nil {
		return fmt.Errorf("attach: %w", err)
	}

	if err := a.writeContextFiles(ctx, emp); err != nil {
		// Context files are advisory for the tool; a failure must not kill
		// the agent loop (e.g. read-only home on some hosts).
		slog.Warn("write context files", "err", err)
	}

	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()
	go a.heartbeat(hbCtx)

	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		a.reapSession() // V-1: reclaim the tool session when idle
		task, err := a.pollTask(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("poll task", "err", err)
			continue
		}
		if task == nil {
			continue
		}
		a.runTask(ctx, *task)
	}
}

func (a *Agent) getMe(ctx context.Context) (*Employee, error) {
	var out struct {
		Employee *Employee `json:"employee"`
	}
	if err := a.do(ctx, "GET", "/api/me", nil, &out); err != nil {
		return nil, err
	}
	if out.Employee == nil {
		return nil, fmt.Errorf("no employee in /api/me response")
	}
	return out.Employee, nil
}

func (a *Agent) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.do(ctx, "POST", "/api/agent/attach", nil, nil); err != nil {
				slog.Warn("heartbeat", "err", err)
			}
		}
	}
}

// pollTask returns the next task to run for this employee, or nil when the
// queue is empty. S18: the server filters to queued tasks (?status=queued);
// the agent picks the highest priority, ties broken by lowest id (FIFO within
// one priority level).
func (a *Agent) pollTask(ctx context.Context) (*Task, error) {
	a.mu.Lock()
	empID := a.empID
	a.mu.Unlock()
	var out struct {
		Tasks []Task `json:"tasks"`
	}
	if err := a.do(ctx, "GET", fmt.Sprintf("/api/tasks?employee=%d&status=queued", empID), nil, &out); err != nil {
		return nil, err
	}
	var best *Task
	for i := range out.Tasks {
		t := &out.Tasks[i]
		if t.Status != "queued" {
			continue // defensive: the server filters, but never run non-queued
		}
		if best == nil || t.Priority > best.Priority || (t.Priority == best.Priority && t.ID < best.ID) {
			best = t
		}
	}
	return best, nil
}

func (a *Agent) buildToolSpec(emp *Employee) tool.Spec {
	name := emp.Tool
	// Auto-detect a real local tool when the configured tool is the default
	// mock. Priority: opencode > claudecode > codex > mock.
	if strings.EqualFold(strings.TrimSpace(name), "mock") || name == "" {
		detect := a.cfg.DetectLocalTool
		if detect == nil {
			detect = tool.DetectLocalTool
		}
		if detected := detect(); detected != "" {
			slog.Info("auto-detected local tool", "tool", detected)
			name = detected
		}
	}
	spec := tool.Spec{
		Name:      name,
		Workdir:   a.resolveWorkdir(emp.Workdir),
		MockSleep: a.cfg.MockSleep,
		Env:       a.toolEnv(),
	}
	if emp.ToolArgs != "" && emp.ToolArgs != "{}" {
		var ta struct {
			Argv []string `json:"argv"`
		}
		if json.Unmarshal([]byte(emp.ToolArgs), &ta) == nil {
			spec.Args = ta.Argv
		}
	}
	return spec
}

// resolveWorkdir resolves the employee workdir under BaseDir with an
// inclusion check (no escapes); falls back to BaseDir on violation.
func (a *Agent) resolveWorkdir(wd string) string {
	if wd == "" {
		return ""
	}
	base := a.cfg.BaseDir
	if !filepath.IsAbs(base) {
		if abs, err := filepath.Abs(base); err == nil {
			base = abs
		}
	}
	abs := wd
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(base, abs)
	}
	if cleaned, err := filepath.Abs(abs); err == nil {
		abs = cleaned
	}
	if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		slog.Warn("workdir escapes base, falling back", "wd", wd, "base", base)
		return base
	}
	_ = os.MkdirAll(abs, 0o755)
	return abs
}
