//go:build !windows

package connections

import (
	"os/exec"
	"syscall"
)

// Starts the child in its own session so it outlives the desktop, as a detached Node spawn does
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
