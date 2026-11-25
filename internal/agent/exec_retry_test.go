package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"whatbark/internal/store"
)

// runAgentWithConfig is like runAgent but accepts a full Config, filling the
// test-only timing/base-dir defaults when left zero so P1a retry knobs can be
// exercised without waiting on the production defaults.
func runAgentWithConfig(t *testing.T, e *env, cfg Config) *Agent {
	t.Helper()
	if cfg.ServerURL == "" {
		cfg.ServerURL = e.srv.URL
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30 * time.Millisecond
	}
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = 10 * time.Second
	}
	if cfg.CancelPoll == 0 {
		cfg.CancelPoll = 50 * time.Millisecond
	}
	if cfg.BaseDir == "" {
		cfg.BaseDir = t.TempDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ag := New(cfg)
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

// writeLLMScript writes a /bin/sh tool script that increments a counter file
// on each invocation. When the attempt number is < atLeast it prints a
// transient LLM-style error and exits 1; at atLeast it prints success and
// exits 0. atLeast <= 0 means the script always fails transiently.
func writeLLMScript(t *testing.T, dir string, atLeast int) (script, counter string) {
	t.Helper()
	counter = filepath.Join(dir, "count")
	script = filepath.Join(dir, "llm.sh")
	body := "#!/bin/sh\n" +
		"c=\"" + counter + "\"\n" +
		"n=$(cat \"$c\" 2>/dev/null || printf 0)\n" +
		"n=$((n+1))\n" +
		"printf %s \"$n\" > \"$c\"\n"
	if atLeast > 0 {
		body += "if [ \"$n\" -lt " + strconv.Itoa(atLeast) + " ]; then\n" +
			"  printf 'LLM API error: 529 overloaded_error, try again later\\n'\n" +
			"  exit 1\nfi\n" +
			"printf 'final report: ok\\n'\n"
	} else {
		body += "printf 'LLM API error: 529 overloaded_error, try again later\\n'\n" +
			"exit 1\n"
	}
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, counter
}

func readCounter(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read counter %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

// TestTransientPatterns pins which failed tool outputs count as transient
// (P1a). Only the last 4KB of output plus the exit error are probed.
func TestTransientPatterns(t *testing.T) {
	exitErr := fmt.Errorf("exit status 1")
	cases := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"no marker", "all good", exitErr, false},
		{"529", "HTTP 529: overloaded", exitErr, true},
		{"overloaded_error", "upstream: overloaded_error", exitErr, true},
		{"api error", "API Error: something", exitErr, true},
		{"rate limit", "429 Too Many Requests: rate limit", exitErr, true},
		{"connection reset", "read tcp: connection reset by peer", exitErr, true},
		{"econnreset", "Error: ECONNRESET", exitErr, true},
		{"etimedout", "Error: ETIMEDOUT", exitErr, true},
		{"socket hang up", "fetch failed (socket hang up)", exitErr, true},
		{"temporarily unavailable", "503 Service Temporarily Unavailable", exitErr, true},
		{"never arrived", "Error: part of the response never arrived", exitErr, true},
		{"case-insensitive", "API ERROR: OVERLOADED_ERROR", exitErr, true},
		{"marker only in error", "", fmt.Errorf("read: ECONNRESET"), true},
		{"marker in tail", strings.Repeat("x", 5000) + " rate limit", exitErr, true},
		{"marker outside tail", "rate limit\n" + strings.Repeat("x", 5000), exitErr, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTransientToolError(c.out, c.err); got != c.want {
				t.Errorf("isTransientToolError outLen=%d err=%v = %v, want %v",
					len(c.out), c.err, got, c.want)
			}
		})
	}
}

// TestAgentRetriesTransient (P1a, A10/A11): a tool that fails transiently on
// attempts 1 and 2 then succeeds on attempt 3 must finish the task, and the
// persisted output must carry both retry notes plus the success line.
func TestAgentRetriesTransient(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)

	script, counter := writeLLMScript(t, t.TempDir(), 3)
	if err := e.st.SetTool(empID, script, "{}"); err != nil {
		t.Fatal(err)
	}
	taskID := dispatch(t, e, empID, "do the thing")

	runAgentWithConfig(t, e, Config{
		Token:           tok,
		LLMMaxRetries:   2,
		LLMRetryBackoff: 50 * time.Millisecond,
	})

	tk := waitTaskStatus(t, e.st, taskID, store.TaskDone, 15*time.Second)
	if !strings.Contains(tk.Output, "transient tool failure (attempt 1/3)") {
		t.Errorf("output missing retry note 1: %q", tk.Output)
	}
	if !strings.Contains(tk.Output, "transient tool failure (attempt 2/3)") {
		t.Errorf("output missing retry note 2: %q", tk.Output)
	}
	if !strings.Contains(tk.Output, "final report: ok") {
		t.Errorf("output missing success marker: %q", tk.Output)
	}
	if got := readCounter(t, counter); got != "3" {
		t.Errorf("attempts = %q, want 3", got)
	}
}

// TestAgentExhaustsRetries (P1a): a tool that always fails transiently must
// exhaust the retry budget and leave the task failed, with exactly the number
// of retry notes equal to LLMMaxRetries and a total of 1+LLMMaxRetries runs.
func TestAgentExhaustsRetries(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)

	script, counter := writeLLMScript(t, t.TempDir(), 0)
	if err := e.st.SetTool(empID, script, "{}"); err != nil {
		t.Fatal(err)
	}
	taskID := dispatch(t, e, empID, "do the thing")

	runAgentWithConfig(t, e, Config{
		Token:           tok,
		LLMMaxRetries:   2,
		LLMRetryBackoff: 50 * time.Millisecond,
	})

	tk := waitTaskStatus(t, e.st, taskID, store.TaskFailed, 15*time.Second)
	if n := strings.Count(tk.Output, "transient tool failure (attempt"); n != 2 {
		t.Errorf("retry notes = %d, want 2: %q", n, tk.Output)
	}
	if strings.Contains(tk.Output, "final report: ok") {
		t.Error("failed task output contains success marker")
	}
	if got := readCounter(t, counter); got != "3" {
		t.Errorf("attempts = %q, want 3", got)
	}
}

// TestAgentNoRetryOnNonTransient pins the negative case: a deterministic
// failure (no transient marker) must not be retried.
func TestAgentNoRetryOnNonTransient(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)

	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := filepath.Join(dir, "tool.sh")
	content := "#!/bin/sh\n" +
		"c=\"" + counter + "\"\n" +
		"n=$(cat \"$c\" 2>/dev/null || printf 0)\n" +
		"n=$((n+1))\n" +
		"printf %s \"$n\" > \"$c\"\n" +
		"printf 'deterministic failure: bad config\\n'\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetTool(empID, script, "{}"); err != nil {
		t.Fatal(err)
	}
	taskID := dispatch(t, e, empID, "do the thing")

	runAgentWithConfig(t, e, Config{
		Token:           tok,
		LLMMaxRetries:   2,
		LLMRetryBackoff: 50 * time.Millisecond,
	})

	tk := waitTaskStatus(t, e.st, taskID, store.TaskFailed, 15*time.Second)
	if strings.Contains(tk.Output, "transient tool failure") {
		t.Errorf("non-transient failure was retried: %q", tk.Output)
	}
	if !strings.Contains(tk.Output, "deterministic failure: bad config") {
		t.Errorf("missing tool output: %q", tk.Output)
	}
	if got := readCounter(t, counter); got != "1" {
		t.Errorf("attempts = %q, want 1 (no retry)", got)
	}
}
