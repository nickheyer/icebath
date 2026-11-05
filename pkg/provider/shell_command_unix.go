//go:build !windows

package provider

import (
	"context"
	"os/exec"
)

// Command running the line through /bin/sh -c like subprocess shell mode on POSIX
func pyShellCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "/bin/sh", "-c", command), nil
}
