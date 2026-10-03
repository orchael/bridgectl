//go:build !windows

package bridge

import (
	"errors"
	"os/exec"
	"syscall"
)

// processAlive reports whether pid refers to a running process. Checking for
// EPERM (not just a nil error) lets callers detect processes owned by
// another user that cannot be signaled but are still running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// setNewProcessGroup configures cmd so the process it starts becomes the
// leader of a new process group, letting terminateProcessGroup/
// killProcessGroup signal the whole tree via the negative-pid convention.
func setNewProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		cmd.SysProcAttr.Setpgid = true
	}
}

// terminateProcessGroup asks the process group to exit gracefully.
func terminateProcessGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
}

// killProcessGroup forcibly terminates the process group.
func killProcessGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
