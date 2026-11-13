//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

// Process attributes of cmd, created when unset
func attrs(cmd *exec.Cmd) *syscall.SysProcAttr {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	return cmd.SysProcAttr
}

// Makes the child lead a new process group, out of reach of the terminal interrupt and killable as one group
func NewGroup(cmd *exec.Cmd) {
	a := attrs(cmd)
	if a.Setsid {
		return
	}
	a.Setpgid, a.Pgid = true, 0
}

// Makes the child lead a new session and process group, detached from the controlling terminal
func NewSession(cmd *exec.Cmd) {
	a := attrs(cmd)
	a.Setsid, a.Setpgid, a.Pgid = true, false, 0
}

// Console windows exist only on Windows, so a POSIX child never shows one
func Hide(cmd *exec.Cmd) {}

// Starts cmd in a new session so it outlives this process, its terminal and its process group
func StartDetached(cmd *exec.Cmd) error {
	NewSession(cmd)
	return cmd.Start()
}

// Command that runs line through /bin/sh -c
func ShellCommand(line string) (*exec.Cmd, error) {
	return exec.Command("/bin/sh", "-c", line), nil
}
