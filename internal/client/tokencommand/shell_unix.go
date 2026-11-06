//go:build !windows

package tokencommand

import (
	"os/exec"
	"syscall"
)

func shellCommand() (string, []string) { return "/bin/sh", []string{"-c"} }

// setProcessGroup runs the command in its own process group and makes the
// timeout kill the whole group.
func setProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		killGroup(command)

		return nil
	}
}

func killGroup(command *exec.Cmd) {
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}
