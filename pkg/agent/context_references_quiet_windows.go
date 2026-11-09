//go:build windows

package agent

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Keeps the git and rg children from flashing a console window
func hideConsoleWindow(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.HideWindow = true
	c.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
