// Package tool spawns and supervises local tool processes (mock / opencode /
// claudecode / arbitrary commands) on behalf of the agent.
//
// Cancellation semantics (TASK.md 0b.8): SIGTERM to the whole process group,
// SIGKILL after a grace period. Every tool runs in its own process group so
// shell children die with it. On Windows (no process groups) cancel signals
// the direct child only — see process_windows.go.
package tool

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// Well-known tool argv templates. {prompt} is replaced with the task body.
var builtinArgs = map[string][]string{
	"opencode":   {"opencode", "run", "{prompt}"},
	"claudecode": {"claude", "-p", "{prompt}"},
	"codex":      {"codex", "exec", "{prompt}"},
}

// ErrToolNotFound indicates the tool binary is missing from PATH.
var ErrToolNotFound = errors.New("tool binary not found")

// ErrCanceled is returned by Wait when Cancel was invoked.
var ErrCanceled = errors.New("tool canceled")

// DefaultGrace is how long Cancel waits after SIGTERM before SIGKILL.
const DefaultGrace = 5 * time.Second

// Spec describes how to run a tool.
type Spec struct {
	// Name is "mock" (built-in), a well-known tool, or a program name/path.
	Name string
	// Args is an argv template, used only for custom (non-builtin) tools.
	// A "{prompt}" placeholder is replaced; without it the prompt is appended.
	Args []string
	// Workdir is the process working directory ("" = inherit agent's).
	Workdir string
	// MockSleep controls the built-in mock tool's duration (default 100ms).
	MockSleep time.Duration
	// Env is the full environment for the spawned process (nil = inherit
	// agent's environment via exec's default). The agent sets this to inject
	// BARK_TOKEN / BARK_SERVER for tools that call the bark CLI (S18).
	Env []string
}

// Result is the outcome of a tool run.
type Result struct {
	Output string
	// Err is nil when the tool exited 0 and was not canceled.
	Err error
}

// Process is a running (or finished) tool invocation.
type Process struct {
	mu       sync.Mutex
	out      *bytes.Buffer
	canceled bool
	finished bool

	cmd      *exec.Cmd
	isMock   bool
	cancelCh chan struct{}
	done     chan Result
	closed   chan struct{}

	onLine   func(string)
	grace    time.Duration
	pidReady chan struct{}
	pid      int
	pumpWG   sync.WaitGroup // pump goroutine done (S18: wait before finalizing)
}

// Start launches the tool for the given prompt. It returns immediately;
// use Wait for the final result and Cancel to stop the process tree early.
// onLine (if non-nil) receives each output line as it arrives.
func Start(spec Spec, prompt string, onLine func(string)) (*Process, error) {
	p := &Process{
		out:      &bytes.Buffer{},
		cancelCh: make(chan struct{}),
		done:     make(chan Result, 1),
		closed:   make(chan struct{}),
		onLine:   onLine,
		grace:    DefaultGrace,
		pidReady: make(chan struct{}),
	}
	if strings.EqualFold(strings.TrimSpace(spec.Name), "mock") {
		p.isMock = true
		close(p.pidReady)
		sleep := spec.MockSleep
		if sleep <= 0 {
			sleep = 100 * time.Millisecond
		}
		go p.runMock(prompt, sleep)
		return p, nil
	}

	argv, err := resolveArgs(spec, prompt)
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return nil, fmt.Errorf("%w: %s (not in PATH)", ErrToolNotFound, argv[0])
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = spec.Workdir
	cmd.Env = childEnv(spec.Env, spec.Workdir)
	setPgid(cmd)
	rd, wr, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("output pipe: %w", err)
	}
	cmd.Stdout = wr
	cmd.Stderr = wr // one merged stream: simpler audit, no interleaving

	p.cmd = cmd
	if err := cmd.Start(); err != nil {
		_ = wr.Close()
		_ = rd.Close()
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	_ = wr.Close() // child inherits fds; EOF after it exits
	p.pid = cmd.Process.Pid
	close(p.pidReady)

	p.pumpWG.Add(1)
	go p.pump(rd)
	go p.supervise()
	return p, nil
}

// resolveArgs expands the argv template for non-mock tools.
func resolveArgs(spec Spec, prompt string) ([]string, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return nil, errors.New("tool name is empty")
	}
	if argv, ok := builtinArgs[strings.ToLower(name)]; ok {
		return expand(argv, prompt), nil
	}
	return append([]string{name}, expand(spec.Args, prompt)...), nil
}

// expand substitutes the {prompt} placeholder; appends prompt if absent.
func expand(args []string, prompt string) []string {
	out := make([]string, 0, len(args)+1)
	saw := false
	for _, a := range args {
		if strings.Contains(a, "{prompt}") {
			out = append(out, strings.ReplaceAll(a, "{prompt}", prompt))
			saw = true
		} else {
			out = append(out, a)
		}
	}
	if !saw {
		out = append(out, prompt)
	}
	return out
}

// childEnv builds the environment for a spawned tool process: spec.Env when
// set, otherwise the agent's own environment (exec's default inheritance).
// When dir != "" the PWD variable is forced to dir: some CLIs (opencode)
// read $PWD instead of the process cwd, and a stale PWD inherited from the
// agent's launch shell would otherwise send the tool to the agent's launch
// directory instead of the employee workdir.
func childEnv(specEnv []string, dir string) []string {
	env := specEnv
	if len(env) == 0 {
		env = os.Environ()
	}
	if dir == "" {
		return env
	}
	env = append([]string(nil), env...)
	for i, kv := range env {
		if strings.HasPrefix(kv, "PWD=") {
			env[i] = "PWD=" + dir
			return env
		}
	}
	return append(env, "PWD="+dir)
}

// Pid returns the process id (0 for the built-in mock).
func (p *Process) Pid() int {
	<-p.pidReady
	return p.pid
}

// Wait blocks until the tool finishes and returns the merged output.
func (p *Process) Wait() Result {
	res := <-p.done
	p.mu.Lock()
	canceled := p.canceled
	p.mu.Unlock()
	if canceled {
		res.Err = ErrCanceled
	}
	return res
}

// Cancel stops the tool: SIGTERM to the process group, SIGKILL after grace.
// Safe to call multiple times and after normal exit.
func (p *Process) Cancel() {
	p.mu.Lock()
	if p.canceled {
		p.mu.Unlock()
		return
	}
	p.canceled = true
	isMock := p.isMock
	proc := p.cmd
	p.mu.Unlock()

	if isMock {
		close(p.cancelCh)
		return
	}
	if proc == nil || proc.Process == nil {
		return
	}
	pgid := proc.Process.Pid
	if err := killProcessGroup(pgid, syscall.SIGTERM); err != nil && !isGone(err) {
		return // already dead
	}
	select {
	case <-p.closed:
	case <-time.After(p.grace):
		_ = killProcessGroup(pgid, syscall.SIGKILL)
	}
}

// isGone reports whether a kill error means the process is already gone.
func isGone(err error) bool {
	return errors.Is(err, syscall.ESRCH) || strings.Contains(err.Error(), "already a child")
}

// runMock executes the built-in deterministic tool: three spaced lines.
func (p *Process) runMock(prompt string, sleep time.Duration) {
	lines := []string{
		"mock: start",
		fmt.Sprintf("mock: working on %q", truncate(prompt, 80)),
		"mock: done",
	}
	step := sleep / time.Duration(len(lines))
	for _, line := range lines {
		if step > 0 {
			select {
			case <-p.cancelCh:
				p.finish()
				return
			case <-time.After(step):
			}
		}
		p.appendLine(line)
	}
	p.finishWith(nil)
}

func (p *Process) pump(r io.ReadCloser) {
	defer func() { _ = r.Close(); p.pumpWG.Done() }()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		p.appendLine(sc.Text())
	}
}

func (p *Process) supervise() {
	err := p.cmd.Wait()
	if p.cmd.ProcessState != nil && p.cmd.ProcessState.Success() {
		err = nil
	}
	// Wait for pump to drain the stdout pipe before finalizing so the
	// captured output is complete (S18: multi-line tool output must not be
	// truncated by the finish race).
	p.pumpWG.Wait()
	p.finishWith(err)
}

func (p *Process) appendLine(line string) {
	p.mu.Lock()
	_, _ = p.out.WriteString(line + "\n")
	p.mu.Unlock()
	if p.onLine != nil {
		p.onLine(line)
	}
}

func (p *Process) finish() { p.finishWith(ErrCanceled) }

func (p *Process) finishWith(err error) {
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		return
	}
	p.finished = true
	res := Result{Output: p.out.String(), Err: err}
	p.mu.Unlock()
	close(p.closed)
	p.done <- res
}

// truncate returns the first n bytes of s plus an ellipsis, backing off off
// any cut point that would start mid-rune so the prefix stays valid UTF-8.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "..."
}
