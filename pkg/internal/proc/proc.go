// Process group control shared by the shell, hook and MCP child processes
package proc

import (
	"context"
	"os/exec"
)

// Puts the command in its own process group so Kill can reach every descendant
func Group(cmd *exec.Cmd) { group(cmd) }

// Kills the command together with every process in its group
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return kill(cmd)
}

// Command that runs line through the platform shell, bash or sh on Unix and bash or PowerShell on Windows
func ShellCommand(ctx context.Context, line string) *exec.Cmd {
	return shellCommand(ctx, line)
}
