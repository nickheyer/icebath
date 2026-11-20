//go:build !windows

package tools

import "os/exec"

// Niced git command line, the repack must never starve the agents sharing the clone
func boundedGitCommand(args []string) *exec.Cmd {
	return exec.Command("nice", append([]string{"-n", "19", "git"}, args...)...)
}
