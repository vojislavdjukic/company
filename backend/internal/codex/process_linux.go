//go:build linux

package codex

import (
	"os/exec"
	"syscall"
)

// configureCommandProcessGroup gives app-server and any launcher child a
// dedicated process group so shutdown can terminate both.
// command is the process command configured by the Codex client.
func configureCommandProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateCommandProcessGroup sends SIGKILL to the app-server process group.
// command is the Codex process command whose group should be terminated.
func terminateCommandProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
