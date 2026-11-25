package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"whatbark/internal/store"
)

// setShTool points the employee's tool at a shell script (absolute path as
// tool name, argv template ["{prompt}"]) so the full prompt arrives as $1
// without shell word-splitting.
func setShTool(t *testing.T, e *env, empID int, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tool.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetTool(empID, path, `{"argv":["{prompt}"]}`); err != nil {
		t.Fatalf("SetTool: %v", err)
	}
}

// dispatchPrio sends a dispatch with an explicit priority; returns the task id.
func dispatchPrio(t *testing.T, e *env, empID int, body string, prio int) int {
	t.Helper()
	code, got := req(t, http.MethodPost, e.srv.URL+"/api/messages", adminTok,
		map[string]any{"to": empID, "kind": "dispatch", "body": body, "priority": prio})
	if code != http.StatusCreated {
		t.Fatalf("dispatch prio %d: %d %v", prio, code, got)
	}
	return int(got["task_id"].(float64))
}

// TestAgentPreemption (S18): a strictly higher-priority queued task preempts
// the running one. Proof is timestamp-based (robust under -race + parallel
// suite load, where wall-clock deltas are not): with preemption the fast task
// finishes a few seconds after the slow task's started_at; without it the
// fast task cannot start until the slow tool's full first run ends. The slow
// task's started_at is also reset by the re-run (final start >= fast start),
// and it carries the "preempted:" audit note until its re-run finishes.
func TestAgentPreemption(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	// 20s first run gives a wide, load-tolerant timing window: with working
	// preemption the fast task lands a few seconds inside it (even under
	// -race + parallel-suite load); without preemption the fast task waits
	// out all 20s, so the two regimes are separated by ~10s either way.
	setShTool(t, e, empID, `case "$1" in
  *slow*) echo slow-start; sleep 20; echo slow-done ;;
  *) echo fast-done ;;
esac`)

	runAgent(t, e, tok, 100*time.Millisecond)

	slow := dispatchPrio(t, e, empID, "please do the slow thing", 0)
	slowT0 := waitTaskStatus(t, e.st, slow, store.TaskRunning, 8*time.Second)
	if slowT0.StartedAt == nil {
		t.Fatal("slow task has no started_at")
	}

	fast := dispatchPrio(t, e, empID, "urgent fast thing", 9)
	// 30s covers the broken-preemption path too (fast then waits out the full
	// 20s first run); the timestamp assertion below is what discriminates.
	fastT := waitTaskStatus(t, e.st, fast, store.TaskDone, 30*time.Second)
	if fastT.FinishedAt == nil || fastT.StartedAt == nil {
		t.Fatalf("missing timestamps: fast=%+v", fastT)
	}
	if !strings.Contains(fastT.Output, "fast-done") {
		t.Errorf("fast output = %q, want fast-done", fastT.Output)
	}
	// Preemption proof: fast must finish before the slow tool's 20s first run
	// could have completed. Timestamps are second-granular RFC3339 UTC:
	// with preemption fastFinish-slowStart lands in ~1-3s, at most a few
	// more under -race + parallel-suite load; without preemption it is
	// >= ~19s (20s sleep minus truncation).
	slowStart, err := time.Parse(time.RFC3339, *slowT0.StartedAt)
	if err != nil {
		t.Fatalf("parse slow started_at %q: %v", *slowT0.StartedAt, err)
	}
	fastEnd, err := time.Parse(time.RFC3339, *fastT.FinishedAt)
	if err != nil {
		t.Fatalf("parse fast finished_at %q: %v", *fastT.FinishedAt, err)
	}
	if !fastEnd.Before(slowStart.Add(10 * time.Second)) {
		t.Errorf("fast task finished at %s, more than 10s after slow started at %s: preemption not effective (without it the fast task would wait ~20s for the slow first run)",
			*fastT.FinishedAt, *slowT0.StartedAt)
	}

	// While the slow task is re-queued / being re-run it still carries the
	// preempt note (finish overwrites output only at done/failed).
	slowT, err := e.st.GetTask(slow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(slowT.Output, "preempted:") {
		t.Errorf("slow output right after preempt = %q, want 'preempted:' note", slowT.Output)
	}
	if !strings.Contains(slowT.Output, fmt.Sprintf("priority %d", 9)) {
		t.Errorf("slow preempt note = %q, want it to name the intruder's priority", slowT.Output)
	}

	// The slow task's re-run starts strictly after the fast task started:
	// its first run was killed and started_at reset (RFC3339 UTC strings
	// compare lexicographically).
	finalSlow := waitTaskStatus(t, e.st, slow, store.TaskDone, 30*time.Second)
	if finalSlow.StartedAt == nil || fastT.StartedAt == nil {
		t.Fatalf("missing timestamps on final pass: slow=%+v fast=%+v", finalSlow, fastT)
	}
	if *finalSlow.StartedAt < *fastT.StartedAt {
		t.Errorf("slow final started_at %s before fast started_at %s (no re-run?)",
			*finalSlow.StartedAt, *fastT.StartedAt)
	}
	if !strings.Contains(finalSlow.Output, "slow-done") {
		t.Errorf("slow final output = %q, want slow-done", finalSlow.Output)
	}
}

// TestAgentNoPreemptEqualPriority (S18): equal priorities never preempt;
// tasks run serially in id order without any preempt note.
func TestAgentNoPreemptEqualPriority(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	setShTool(t, e, empID, `echo done`)

	runAgent(t, e, tok, 100*time.Millisecond)

	a := dispatchPrio(t, e, empID, "task a", 3)
	waitTaskStatus(t, e.st, a, store.TaskRunning, 8*time.Second)
	b := dispatchPrio(t, e, empID, "task b", 3)

	aT := waitTaskStatus(t, e.st, a, store.TaskDone, 10*time.Second)
	bT := waitTaskStatus(t, e.st, b, store.TaskDone, 10*time.Second)

	if strings.Contains(aT.Output, "preempted") {
		t.Errorf("task A output = %q, must not be preempted at equal priority", aT.Output)
	}
	if strings.Contains(bT.Output, "preempted") {
		t.Errorf("task B output = %q, must not be preempted at equal priority", bT.Output)
	}
	if aT.FinishedAt == nil || bT.StartedAt == nil {
		t.Fatalf("missing timestamps: a=%+v b=%+v", aT, bT)
	}
	if *aT.FinishedAt > *bT.StartedAt {
		t.Errorf("A finished %s after B started %s (overlap; FIFO by id expected)", *aT.FinishedAt, *bT.StartedAt)
	}
}

// TestAgentBootstrapPrompt (S18): the tool's prompt is prefixed with the
// platform header telling the AI where to read its identity and platform docs.
func TestAgentBootstrapPrompt(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	setShTool(t, e, empID, `printf '%s\n' "$1"`)

	runAgent(t, e, tok, 100*time.Millisecond)

	id := dispatchPrio(t, e, empID, "s18-body-marker", 0)
	tk := waitTaskStatus(t, e.st, id, store.TaskDone, 10*time.Second)
	for _, want := range []string{
		"You are an AI employee of the whatbark platform,",
		"whatbark/identity.md",
		"whatbark/platform.md",
		"---",
		"s18-body-marker",
	} {
		if !strings.Contains(tk.Output, want) {
			t.Errorf("prompt output missing %q; output=%q", want, tk.Output)
		}
	}
}

// TestAgentEnvInjection (S18): spawned tools see BARK_TOKEN and BARK_SERVER
// in their environment (bark CLI usable inside tools).
func TestAgentEnvInjection(t *testing.T) {
	e := newEnv(t)
	empID, tok := makeEmployee(t, e, "E1", store.KindAI)
	setShTool(t, e, empID, `echo "TOKEN=$BARK_TOKEN"; echo "SERVER=$BARK_SERVER"`)

	runAgent(t, e, tok, 100*time.Millisecond)

	id := dispatchPrio(t, e, empID, "env check", 0)
	tk := waitTaskStatus(t, e.st, id, store.TaskDone, 10*time.Second)
	if !strings.Contains(tk.Output, "TOKEN="+tok) {
		t.Errorf("output lacks BARK_TOKEN=%s; output=%q", tok, tk.Output)
	}
	if !strings.Contains(tk.Output, "SERVER="+e.srv.URL) {
		t.Errorf("output lacks BARK_SERVER=%s; output=%q", e.srv.URL, tk.Output)
	}
}

// TestAgentContextFiles (S18): the initial attach writes whatbark/identity.md
// and whatbark/platform.md into the employee workdir, regenerates them on
// re-attach (overwrite), and never touches the user's own role.md.
func TestAgentContextFiles(t *testing.T) {
	e := newEnv(t)
	// A department with a leader so the reporting line is non-empty.
	dept, err := e.st.CreateDepartment("CtxDept", nil)
	if err != nil {
		t.Fatal(err)
	}
	leadID, err := e.st.CreateEmployee("CL", "tester", store.KindAI, dept)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetDepartmentLeader(dept, leadID); err != nil {
		t.Fatal(err)
	}
	empID, err := e.st.CreateEmployee("CE", "tester", store.KindAI, dept)
	if err != nil {
		t.Fatal(err)
	}
	tok := "tok-CE"
	sum := sha256.Sum256([]byte(tok))
	if err := e.st.SetTokenHash(empID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}

	ag := runAgent(t, e, tok, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	emp, err := ag.getMe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wd := ag.resolveWorkdir(emp.Workdir)
	identPath := filepath.Join(wd, contextDirName, "identity.md")
	platformPath := filepath.Join(wd, contextDirName, "platform.md")
	rolePath := filepath.Join(wd, "role.md")

	// The agent writes context files asynchronously after attach; poll until
	// identity.md is complete (ends with the stable platform pointer line).
	deadline := time.Now().Add(5 * time.Second)
	var ident []byte
	for {
		ident, err = os.ReadFile(identPath)
		if err == nil && strings.Contains(string(ident), "whatbark/platform.md") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("identity.md not written under %s: %v", wd, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, want := range []string{"CE", "tester", "CL", "whatbark/platform.md"} {
		if !strings.Contains(string(ident), want) {
			t.Errorf("identity.md missing %q; content=%s", want, ident)
		}
	}
	if strings.Contains(string(ident), "../role.md") {
		t.Errorf("identity.md mentions role.md although it does not exist; content=%s", ident)
	}
	var platform []byte
	for {
		platform, err = os.ReadFile(platformPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("platform.md not written under %s: %v", wd, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(platform), "bark") {
		t.Errorf("platform.md missing bark quick-reference; content=%s", platform)
	}
	if _, err := os.Stat(rolePath); !os.IsNotExist(err) {
		t.Errorf("agent must not create role.md (stat err=%v)", err)
	}

	// Create the user's role.md, then re-attach (initial): identity.md must be
	// regenerated to mention it, and role.md itself must stay untouched.
	if err := os.WriteFile(rolePath, []byte("ROLE-XYZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag2 := New(Config{
		ServerURL:    e.srv.URL,
		Token:        tok,
		PollInterval: 30 * time.Millisecond,
		Heartbeat:    10 * time.Second,
		CancelPoll:   50 * time.Millisecond,
		BaseDir:      ag.cfg.BaseDir,
		MockSleep:    100 * time.Millisecond,
		// Hermetic: never auto-detect a real local tool in tests.
		DetectLocalTool: func() string { return "" },
	})
	ctx2, cancel2 := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- ag2.Run(ctx2) }()
	time.Sleep(400 * time.Millisecond) // let the initial attach + file write land
	cancel2()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("second agent Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second agent did not stop within 10s of cancel")
	}

	ident2, err := os.ReadFile(identPath)
	if err != nil {
		t.Fatalf("identity.md missing after re-attach: %v", err)
	}
	if !strings.Contains(string(ident2), "../role.md") {
		t.Errorf("identity.md not regenerated to mention role.md after re-attach; content=%s", ident2)
	}
	after, err := os.ReadFile(rolePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "ROLE-XYZ" {
		t.Errorf("role.md modified by agent: %q", after)
	}
}
