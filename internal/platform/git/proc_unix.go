//go:build !windows

package git

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the child in its own process group.
//
// Without it, a signal sent to end the operation reaches only the direct child,
// and a Git that spawned a helper would leave that helper running after the
// request ended (docs/security/threat-model.md T-A8).
func configureProcessGroup(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
}

// killProcessGroup kills the child and everything it started.
//
// The negative identifier targets the process group rather than one process, so
// a helper Git started is reaped with it.
func killProcessGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return errors.New("the Git process has not started")
	}
	pid := command.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	// The group may already be gone; the direct kill is the backstop, and its
	// error is the one worth reporting.
	return command.Process.Kill()
}
