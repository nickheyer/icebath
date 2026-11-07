package media

import (
	"os/exec"
	"syscall"
)

// Adds Win32 creation flags to the child spawn
func applyCreationFlags(cmd *exec.Cmd, flags uint32) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= flags
}

// Hides the console of a short lived helper without detaching it from the job
func hideConsoleWindow(cmd *exec.Cmd) { applyCreationFlags(cmd, windowsCreateNoWindow) }
