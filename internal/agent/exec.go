package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"whatbark/internal/tool"
)

const maxOutput = 64 * 1024

// runTask claims and executes one task: start -> session begin -> tool ->
// output stream -> finish/fail. Cancellation and preemption (S18) are
// detected by polling the task's server status; on preempt the server has
// already requeued the task, so the agent must NOT finish/fail it.
//
// V-1: the tool runs inside the per-employee session. For the mock tool the
// session persists across consecutive tasks (no re-init, cross-task context);
// for CLI tools each task is a one-shot session. Cancel/preempt paths record
// Finish(canceled=true) so the session survives and S18/S27 semantics are
// unchanged.
//
// P1a: transient tool failures (LLM rate limits, 529, overloaded_error,
// network resets) are retried in place with exponential backoff, up to
// LLMMaxRetries times. The session Begin/Finish pair still spans the whole
// task (one Begin, one Finish on every terminal path).
func (a *Agent) runTask(ctx context.Context, task Task) {
	if err := a.do(ctx, "POST", fmt.Sprintf("/api/tasks/%d/start", task.ID), nil, nil); err != nil {
		slog.Warn("start task", "id", task.ID, "err", err)
		return
	}

	a.mu.Lock()
	spec := a.spec
	if a.session == nil || a.session.ToolName() != spec.Name {
		// First task, or the tool was changed by an admin mid-lifetime:
		// (re)create the session bound to the current tool.
		a.session = NewSession(spec, a.cfg.SessionIdle, a.cfg.SessionMaxFailures)
	}
	se := a.session
	emp := a.emp
	a.mu.Unlock()

	prompt := task.Input
	if emp != nil {
		prompt = a.bootstrapPrompt(emp, task.Input)
	}

	// The session header is streamed live below AND prepended to the saved
	// report, so task reuse is verifiable in the persisted output (probes).
	gen, header := se.Begin(task.Input)
	slog.Info("task running", "id", task.ID, "session_gen", gen)
	if header != "" {
		a.postOutput(ctx, task.ID, header)
	}

	// P1a: retry policy. Zero config values take the New() defaults
	// (2 retries, 2s initial backoff) — see Config doc.
	maxAttempts := 1 + a.cfg.LLMMaxRetries
	backoff := a.cfg.LLMRetryBackoff

	// One watcher per task, spanning all retry attempts: cancel/preempt
	// signals raised during a backoff window must reach the backoff select
	// below, not a stale per-attempt watcher.
	cancelCh := make(chan struct{})
	preemptCh := make(chan struct{})
	go a.watchCancel(ctx, task, cancelCh, preemptCh)

	// transcript accumulates the live-streamed retry notes so the persisted
	// final report (finish/fail output) carries the retry history too.
	var transcript string

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		proc, err := tool.Start(spec, prompt, func(line string) {
			a.postOutput(ctx, task.ID, line)
		})
		if err != nil {
			// Start failure (binary missing, argv template broken, ...):
			// deterministic — retrying cannot help, fail the task now.
			se.Finish(true, false)
			a.failTask(ctx, task.ID, header+transcript+"tool start: "+err.Error())
			return
		}
		a.setActive(proc)

		resultCh := make(chan tool.Result, 1)
		go func() { resultCh <- proc.Wait() }()

		select {
		case r := <-resultCh:
			a.clearActive()
			if r.Err == nil {
				// Upload workdir artifacts BEFORE finishTask: the server only
				// accepts uploads while the task is still running. Log-only
				// failures (artifact reporting must never fail the task).
				a.uploadTaskArtifacts(ctx, task, spec.Workdir)
				a.finishTask(ctx, task.ID, header+tail(transcript+tail(r.Output, maxOutput), maxOutput))
				se.Finish(false, false)
				return
			}
			// P1a: retry only transient failures (rate limit, 529, ...),
			// never cancellations, and only while attempts remain.
			if r.Err != tool.ErrCanceled && isTransientToolError(r.Output, r.Err) && attempt < maxAttempts {
				note := fmt.Sprintf("whatbark: transient tool failure (attempt %d/%d), retrying in %s: %s",
					attempt, maxAttempts, backoff, strings.TrimSpace(tail(r.Output, 256)))
				a.postOutput(ctx, task.ID, note)
				transcript += note + "\n"
				slog.Warn("transient tool failure; retrying", "id", task.ID,
					"attempt", attempt, "of", maxAttempts, "backoff", backoff, "err", r.Err)
				select {
				case <-time.After(backoff):
				case <-cancelCh:
					slog.Info("task canceled by server during backoff", "id", task.ID)
					se.Finish(false, true)
					return
				case <-preemptCh:
					slog.Info("task preempted during backoff", "id", task.ID)
					se.Finish(false, true)
					return
				case <-ctx.Done():
					se.Finish(false, true)
					return
				}
				backoff *= 2
				if backoff > maxRetryBackoff {
					backoff = maxRetryBackoff
				}
				continue
			}
			a.failTask(ctx, task.ID, header+tail(transcript+tail(r.Output, maxOutput), maxOutput)+"\n"+r.Err.Error())
			se.Finish(true, false)
			return
		case <-cancelCh:
			proc.Cancel()
			<-resultCh
			a.clearActive()
			slog.Info("task canceled by server", "id", task.ID)
			se.Finish(false, true)
			return
		case <-preemptCh:
			// Server-side requeue already done; do not finish/fail.
			proc.Cancel()
			<-resultCh
			a.clearActive()
			slog.Info("task preempted by higher priority", "id", task.ID)
			se.Finish(false, true)
			return
		case <-ctx.Done():
			proc.Cancel()
			<-resultCh
			a.clearActive()
			se.Finish(false, true)
			return
		}
	}
}

// maxRetryBackoff caps the P1a exponential backoff between retry attempts.
const maxRetryBackoff = 30 * time.Second

// transientPatterns (P1a) are case-insensitive markers of transient LLM or
// infrastructure failures (rate limiting, 529, overloaded, network resets)
// that a failed tool run may print before dying. The probe only inspects the
// last 4KB of output (LLM CLIs dump the error near the end, and huge dumps
// should not dominate the check) plus the exit error message.
var transientPatterns = []string{
	"part of the response never arrived",
	"overloaded_error",
	"api error",
	"529",
	"econnreset",
	"etimedout",
	"connection reset by peer",
	"socket hang up",
	"temporarily unavailable",
	"rate limit",
}

// isTransientToolError reports whether a failed tool run (err != nil) looks
// like a transient LLM/infrastructure failure worth retrying (P1a).
func isTransientToolError(output string, err error) bool {
	probe := strings.ToLower(tail(output, 4096))
	if err != nil {
		probe += " " + strings.ToLower(err.Error())
	}
	for _, p := range transientPatterns {
		if strings.Contains(probe, p) {
			return true
		}
	}
	return false
}

// setActive / clearActive manage a.active under the agent mutex. Under the
// per-agent single-task model, a task's terminal path directly sets the
// active process to nil (no conditional guard), while the lock keeps the
// pair race-safe with readers such as ActivePID.
func (a *Agent) setActive(p *tool.Process) {
	a.mu.Lock()
	a.active = p
	a.mu.Unlock()
}

func (a *Agent) clearActive() {
	a.mu.Lock()
	a.active = nil
	a.mu.Unlock()
}

// watchCancel polls the task until it reports canceled or preempted, then
// closes the corresponding out channel. It also exits once the task reaches a
// terminal state (done/failed), so it never keeps polling after completion
// (S7-M1 goroutine-leak fix). S18: while the task is still running, each tick
// also scans the queued list; a strictly higher-priority queued task triggers
// a preempt (server requeues this task; agent then picks the new task).
func (a *Agent) watchCancel(ctx context.Context, task Task, cancelCh, preemptCh chan struct{}) {
	ticker := time.NewTicker(a.cfg.CancelPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var t TaskStatus
			if err := a.do(ctx, "GET", fmt.Sprintf("/api/tasks/%d", task.ID), nil, &t); err != nil {
				// S27 R1: if the task is gone or no longer ours (404/403), it has
				// been deleted or reassigned away. Stop polling instead of
				// spinning forever, and cancel the tool so we don't keep
				// executing a task that no longer belongs to us.
				var apiErr *APIError
				if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusForbidden) {
					slog.Warn("task no longer available, treating as preempted",
						"id", task.ID, "status", apiErr.Status)
					close(preemptCh)
					return
				}
				continue
			}
			switch t.Status {
			case "canceled":
				close(cancelCh)
				return
			case "done", "failed":
				return // terminal: result handled by runTask's select
			case "queued":
				// Requeued by a preempt (this agent or an external caller):
				// stop the tool; the task will be picked up again later.
				close(preemptCh)
				return
			}
			if a.preemptIfHigher(ctx, task) {
				close(preemptCh)
				return
			}
		}
	}
}

// preemptIfHigher checks whether a queued task of strictly higher priority
// exists for this employee. If so it POSTs the preempt on the current task
// and reports success (retry on the next tick after failure).
func (a *Agent) preemptIfHigher(ctx context.Context, task Task) bool {
	a.mu.Lock()
	empID := a.empID
	a.mu.Unlock()
	var out struct {
		Tasks []Task `json:"tasks"`
	}
	if err := a.do(ctx, "GET", fmt.Sprintf("/api/tasks?employee=%d&status=queued", empID), nil, &out); err != nil {
		return false
	}
	for i := range out.Tasks {
		t := &out.Tasks[i]
		if t.ID == task.ID || t.Priority <= task.Priority {
			continue
		}
		body := map[string]any{"reason": fmt.Sprintf("task %d (priority %d)", t.ID, t.Priority)}
		if err := a.do(ctx, "POST", fmt.Sprintf("/api/tasks/%d/preempt", task.ID), body, nil); err != nil {
			slog.Warn("preempt", "task", task.ID, "by", t.ID, "err", err)
			return false
		}
		slog.Info("preempting task", "task", task.ID, "by", t.ID, "priority", t.Priority)
		return true
	}
	return false
}

func (a *Agent) postOutput(ctx context.Context, taskID int, line string) {
	body := map[string]any{"task_id": taskID, "line": line}
	if err := a.do(ctx, "POST", "/api/agent/output", body, nil); err != nil {
		slog.Warn("post output", "task", taskID, "err", err)
	}
}

func (a *Agent) finishTask(ctx context.Context, taskID int, output string) {
	body := map[string]any{"output": output}
	if err := a.do(ctx, "POST", fmt.Sprintf("/api/tasks/%d/finish", taskID), body, nil); err != nil {
		slog.Warn("finish task", "id", taskID, "err", err)
	}
}

func (a *Agent) failTask(ctx context.Context, taskID int, output string) {
	body := map[string]any{"output": output}
	if err := a.do(ctx, "POST", fmt.Sprintf("/api/tasks/%d/fail", taskID), body, nil); err != nil {
		slog.Warn("fail task", "id", taskID, "err", err)
	}
}

// TaskStatus is the minimal task status probe response.
type TaskStatus struct {
	Status string `json:"status"`
}

// tail returns the last n bytes of s, advancing the cut start past any
// continuation bytes so the result never begins mid-rune (S7 nit: UTF-8
// safe truncation).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
