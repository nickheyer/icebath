//go:build windows

package proc

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Exit code of a finished process
func ExitCode(state *os.ProcessState) int { return state.ExitCode() }

// Keeps a child from opening a console window of its own, its other attributes left as they are
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
