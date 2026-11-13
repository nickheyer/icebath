//go:build windows

package qqbot

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Child starts without a console window
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
