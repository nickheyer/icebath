//go:build !windows

package builtin

import (
	"context"
	"os/exec"
)

// The claude binary run directly with its arguments
func claudeCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "claude", args...)
}
