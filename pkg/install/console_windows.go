//go:build windows

package install

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Runs stage children without flashing a console window
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
