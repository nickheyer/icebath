//go:build windows

package git

import (
	"os/exec"
	"syscall"
)

// Starts the process hidden with no console window, as windowsHide does
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

// Ends the process the way child.kill ends it on Windows
func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
