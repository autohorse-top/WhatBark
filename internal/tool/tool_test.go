package tool

import (
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestResolveArgs(t *testing.T) {
	cases := []struct {
		name    string
		spec    Spec
		prompt  string
		want    []string
		wantErr bool
	}{
		{name: "opencode builtin", spec: Spec{Name: "opencode"}, prompt: "hi", want: []string{"opencode", "run", "hi"}},
		{name: "claudecode builtin", spec: Spec{Name: "claudecode"}, prompt: "hi", want: []string{"claude", "-p", "hi"}},
		{name: "codex builtin", spec: Spec{Name: "codex"}, prompt: "hi", want: []string{"codex", "exec", "hi"}},
		{name: "custom placeholder", spec: Spec{Name: "sh", Args: []string{"-c", "{prompt}"}}, prompt: "echo hi", want: []string{"sh", "-c", "echo hi"}},
		{name: "custom append", spec: Spec{Name: "sh", Args: []string{"-c"}}, prompt: "echo hi", want: []string{"sh", "-c", "echo hi"}},
		{name: "empty name", spec: Spec{}, prompt: "hi", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveArgs(c.spec, c.prompt)
			if c.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveArgs: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("resolveArgs() = %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("resolveArgs()[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestRunShellEcho(t *testing.T) {
	var mu sync.Mutex
	lines := []string{}
	prompt := "echo alpha\necho beta"
	p, err := Start(Spec{Name: "sh", Args: []string{"-c", "{prompt}"}}, prompt, func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := p.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if !strings.Contains(res.Output, "alpha") || !strings.Contains(res.Output, "beta") {
		t.Fatalf("output = %q, want contains alpha and beta", res.Output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 || lines[0] != "alpha" || lines[1] != "beta" {
		t.Fatalf("lines = %v, want [alpha beta]", lines)
	}
}

func TestRunMock(t *testing.T) {
	var mu sync.Mutex
	lines := []string{}
	p, err := Start(Spec{Name: "mock", MockSleep: 60 * time.Millisecond}, "task body", func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := p.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if !strings.Contains(res.Output, "mock: start") || !strings.Contains(res.Output, "mock: done") {
		t.Fatalf("output = %q, want mock: start and mock: done", res.Output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 3 {
		t.Fatalf("lines = %v, want 3 lines", lines)
	}
}

func TestChildEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     []string
		dir     string
		wantPWD string
	}{
		{name: "PWD forced to workdir when present", env: []string{"A=1", "PWD=/stale", "BARK_TOKEN=t"}, dir: "/tmp/agent/work", wantPWD: "/tmp/agent/work"},
		{name: "PWD appended when absent", env: []string{"A=1"}, dir: "/tmp/agent/work", wantPWD: "/tmp/agent/work"},
		{name: "PWD untouched without workdir", env: []string{"A=1", "PWD=/stale"}, dir: "", wantPWD: "/stale"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := append([]string(nil), c.env...)
			got := childEnv(c.env, c.dir)
			var pwd string
			for _, kv := range got {
				if strings.HasPrefix(kv, "PWD=") {
					pwd = strings.TrimPrefix(kv, "PWD=")
				}
			}
			if pwd != c.wantPWD {
				t.Fatalf("PWD = %q, want %q (env=%v)", pwd, c.wantPWD, got)
			}
			if len(c.env) != len(orig) {
				t.Fatalf("caller env length changed: %d -> %d", len(orig), len(c.env))
			}
			for i := range orig {
				if c.env[i] != orig[i] {
					t.Fatalf("caller env[%d] mutated: %q -> %q", i, orig[i], c.env[i])
				}
			}
		})
	}
	t.Run("nil env falls back to process environment", func(t *testing.T) {
		if got := childEnv(nil, ""); len(got) == 0 {
			t.Fatal("childEnv(nil, \"\") = empty, want fallback to os.Environ()")
		}
	})
}

func TestToolNotFound(t *testing.T) {
	_, err := Start(Spec{Name: "whatbark-no-such-tool-xyz"}, "hi", nil)
	if err == nil {
		t.Fatal("expected tool not found error")
	}
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("err = %v, want ErrToolNotFound", err)
	}
}

func TestCancelKillsProcess(t *testing.T) {
	p, err := Start(Spec{Name: "sh", Args: []string{"-c", "{prompt}"}}, "sleep 31.4 & wait", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := p.Pid()
	time.Sleep(200 * time.Millisecond)
	p.Cancel()
	res := p.Wait()
	if !errors.Is(res.Err, ErrCanceled) {
		t.Fatalf("Wait: err = %v, want canceled", res.Err)
	}
	// Signal 0 probes liveness without sending anything (Go 1.27 removed
	// the os.Kill function in favor of Process.Signal).
	proc, ferr := os.FindProcess(pid)
	if ferr == nil {
		if err := proc.Signal(syscall.Signal(0)); err == nil {
			t.Fatalf("process %d still alive after cancel", pid)
		}
	}
}

func TestCancelMock(t *testing.T) {
	p, err := Start(Spec{Name: "mock", MockSleep: 5 * time.Second}, "x", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	p.Cancel()
	res := p.Wait()
	if !errors.Is(res.Err, ErrCanceled) {
		t.Fatalf("Wait: err = %v, want canceled", res.Err)
	}
}
