//go:build windows

package tokencommand

import "os/exec"

func shellCommand() (string, []string) { return "cmd.exe", []string{"/d", "/c"} }

func setProcessGroup(*exec.Cmd) {}

func killGroup(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}
