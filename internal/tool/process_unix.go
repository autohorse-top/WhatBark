//go:build !windows

package tool

import (
	"os/exec"
	"syscall"
)

// setPgid puts the tool in its own process group so Cancel can signal the
// entire shell subtree (TASK 0b.8).
func setPgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the process group led by pid.
func killProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
