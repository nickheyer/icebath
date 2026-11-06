//go:build windows

package selfupdate

import (
	"os/exec"
	"syscall"
)

// Starts a git probe without a console window
func hideProbeWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
