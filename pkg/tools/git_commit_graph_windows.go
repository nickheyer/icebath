//go:build windows

package tools

import (
	"os/exec"
	"syscall"
	"time"
)

// Git command in its own console process group so the timeout can take the whole tree down
func boundedGitCommand(args []string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	hideConsoleWindow(cmd)
	return cmd
}

// Windows always forces, the whole tree goes down at once
func terminateProcessTreeGrace(cmd *exec.Cmd, _ <-chan struct{}, _ time.Duration) {
	KillProcessTree(cmd.Process.Pid)
}
