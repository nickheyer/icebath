//go:build windows

package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Command running a bootstrap line through ComSpec with a hidden window like subprocess shell mode
func bootstrapShell(ctx context.Context, command string) *exec.Cmd {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	cmd := exec.CommandContext(ctx, comspec)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: fmt.Sprintf(`%s /c "%s"`, comspec, command), HideWindow: true}
	return cmd
}
