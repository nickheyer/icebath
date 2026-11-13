//go:build !windows

package git

import (
	"os/exec"
	"syscall"
)

// No window exists to hide off Windows
func hideWindow(*exec.Cmd) {}

// Sends SIGTERM, the signal execFile kills with at its deadline
func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Signal(syscall.SIGTERM)
	}
}
