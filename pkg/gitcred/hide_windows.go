//go:build windows

package gitcred

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Starts the helper without flashing a console window
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
