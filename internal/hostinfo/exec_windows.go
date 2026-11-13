package hostinfo

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Starts the program without a console window so a windowless backend never flashes one
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
