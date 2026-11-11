//go:build windows

package dashboard

import (
	"os/exec"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Command of an argv with the command line Popen builds through list2cmdline
func popenCommand(argv []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: pysubprocess.List2cmdline(argv)}
	return cmd
}
