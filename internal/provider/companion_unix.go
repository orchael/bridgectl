//go:build !windows

package provider

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Launchers such as codex.js spawn the actual server as a child. Keep the
// companion in its own group so cancellation also releases the child's locks.
func configureCompanionProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killCompanionProcess(cmd) }
}

func killCompanionProcess(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
