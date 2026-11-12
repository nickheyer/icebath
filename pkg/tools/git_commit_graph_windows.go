//go:build windows

package tools

import (
	"os/exec"
	"syscall"
)

// Git command in its own console process group so the timeout can take the whole tree down
func boundedGitCommand(args []string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	hideConsoleWindow(cmd)
	return cmd
}
