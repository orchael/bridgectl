//go:build windows

package bridge

import (
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

// stillActive is the Win32 STILL_ACTIVE sentinel (259 / 0x103) returned by
// GetExitCodeProcess for a process that has not yet exited. It is not
// exported by golang.org/x/sys/windows, so it is defined here directly.
const stillActive = 259

// processAlive reports whether pid refers to a running process. Windows has
// no signal-0 liveness probe, so this opens a handle and inspects the exit
// code directly instead.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}

// setNewProcessGroup is a no-op on Windows: this build does not attempt
// Job-Object-based tree supervision (see terminateProcessGroup/
// killProcessGroup below), so there is no process group to create.
func setNewProcessGroup(*exec.Cmd) {}

// terminateProcessGroup has no graceful equivalent to SIGTERM here (no Job
// Object or CTRL_BREAK_EVENT handling in this build), so it falls straight
// through to killProcessGroup rather than waiting out Stop's grace period.
func terminateProcessGroup(pid int) {
	killProcessGroup(pid)
}

// killProcessGroup terminates only the tracked process, not its full
// descendant tree (no process-group/Job-Object support in this build).
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
}
