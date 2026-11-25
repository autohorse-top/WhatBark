//go:build windows

package tool

import (
	"os"
	"os/exec"
	"syscall"
)

// setPgid is a no-op on Windows: the platform has no process groups, so
// Cancel() can only signal the direct child.
func setPgid(cmd *exec.Cmd) {
	// Intentionally empty (documented degradation; Unix keeps group-kill).
}

// killProcessGroup is best-effort on Windows (no process groups):
//
//	SIGTERM: Process.Signal(os.Interrupt) is NOT implemented on Windows
//	(os/exec_windows.go returns EWINDOWS for any signal other than Kill),
//	so the graceful step is a documented no-op; Cancel() then falls
//	through to the grace timeout and hard-kills below.
//	SIGKILL: TerminateProcess on the direct child — the only reliable
//	kill path. Shell children spawned by the tool may outlive it.
func killProcessGroup(pid int, sig syscall.Signal) error {
	if sig == syscall.SIGKILL {
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return p.Kill()
	}
	return nil
}
