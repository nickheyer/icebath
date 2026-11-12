package winenv

import (
	"os/exec"
	"syscall"
)

// Starts the child without a console window as windowsHide does
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
