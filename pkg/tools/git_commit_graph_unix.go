//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Niced git command line, the repack must never starve the agents sharing the clone
func boundedGitCommand(args []string) *exec.Cmd {
	return exec.Command("nice", append([]string{"-n", "19", "git"}, args...)...)
}

// Terminates the tree so git can delete its lock and tmp files, killing what outlives the grace
func terminateProcessTreeGrace(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) {
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	leadsGroup := err == nil && pgid == pid
	if leadsGroup {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	if table, err := proc.Snapshot(); err == nil {
		for _, descendant := range table.Descendants(pid) {
			_ = proc.Terminate(descendant)
		}
	}
	_ = proc.Terminate(pid)
	select {
	case <-done:
		if leadsGroup {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		return
	case <-time.After(grace):
	}
	KillProcessTree(pid)
}
