//go:build !linux

package codex

import "os/exec"

// configureCommandProcessGroup leaves process setup unchanged outside Linux.
// command is the Codex process command.
func configureCommandProcessGroup(command *exec.Cmd) {}

// terminateCommandProcessGroup terminates the Codex process on platforms
// without the Linux process-group implementation.
// command is the Codex process command to terminate.
func terminateCommandProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}
