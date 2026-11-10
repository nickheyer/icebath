//go:build windows

package gateway

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Starts the command without a console window like windows_hide_flags
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
