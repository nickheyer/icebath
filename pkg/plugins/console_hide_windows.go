//go:build windows

package plugins

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Child starts without a console window while staying in the parent process group and job
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
