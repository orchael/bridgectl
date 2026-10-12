package provider

import "os/exec"

func configureCompanionProcess(cmd *exec.Cmd) {}

func killCompanionProcess(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}
