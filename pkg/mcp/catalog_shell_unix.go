//go:build !windows

package mcp

import (
	"context"
	"os/exec"
)

// Command running a bootstrap line through /bin/sh like subprocess shell mode
func bootstrapShell(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}
