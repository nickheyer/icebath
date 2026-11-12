package connections

import (
	"os/exec"
	"syscall"
)

// Starts the child without a console window the way Node windowsHide does
func hideChildWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
