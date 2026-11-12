//go:build !windows

package files

import (
	"os/exec"
	"syscall"
)

// A timed command leads its own process group so its descendants die with it
func configureGitProcess(cmd *exec.Cmd, _ GitSpawnPlan, timed bool) {
	if timed {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
}

// No console exists to hide off Windows
func hideConsole(*exec.Cmd) {}

// SIGKILL to the process group, the leader alone when the group is gone
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		cmd.Process.Kill()
	}
}

func killTrees(cmds []*exec.Cmd) {
	for _, cmd := range cmds {
		killTree(cmd)
	}
}
