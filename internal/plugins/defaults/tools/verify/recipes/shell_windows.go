//go:build windows

package recipes

import (
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/proc"
)

// Shell command of a recipe line, given to the command interpreter as it is
func shellCommand(command string) *exec.Cmd {
	shell := os.Getenv("ComSpec")
	if shell == "" {
		shell = `C:\Windows\System32\cmd.exe`
	}
	program := shell
	if strings.ContainsAny(shell, " \t") {
		program = `"` + shell + `"`
	}
	cmd := exec.Command(shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: program + ` /c "` + command + `"`}
	proc.HideWindow(cmd)
	return cmd
}
