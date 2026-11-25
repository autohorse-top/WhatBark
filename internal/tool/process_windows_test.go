//go:build windows

package tool

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestWindowsKillProcessGroup pins the documented Windows cancel contract:
// the graceful step (SIGTERM) is a no-op that returns nil, and the hard kill
// (SIGKILL) actually terminates the child.
func TestWindowsKillProcessGroup(t *testing.T) {
	// ~30s ping so the child is alive long enough for the assertions.
	cmd := exec.Command("cmd", "/c", "ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Graceful step: documented best-effort no-op on Windows; must not error.
	if err := killProcessGroup(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM step returned %v; expected nil (Interrupt unimplemented on Windows)", err)
	}

	// Hard kill must terminate the child.
	if err := killProcessGroup(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL step returned: %v", err)
	}

	done := make(chan error, 1)
	go func() { _, err := cmd.Process.Wait(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("child exited normally; expected forced termination")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit within 10s after SIGKILL")
	}
}

// TestWindowsSetPgidNoPan pins that setPgid is a safe no-op on Windows.
func TestWindowsSetPgidNoPan(t *testing.T) {
	cmd := exec.Command("true") // never executed
	setPgid(cmd)                // must not panic
}
