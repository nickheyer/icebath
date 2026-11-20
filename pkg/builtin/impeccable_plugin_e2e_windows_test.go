//go:build windows

package builtin

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// The claude cmd shim run through the command interpreter with every argument double quoted
func claudeCommand(ctx context.Context, args ...string) *exec.Cmd {
	parts := []string{"claude"}
	for _, arg := range args {
		parts = append(parts, `"`+strings.ReplaceAll(arg, `"`, `""`)+`"`)
	}
	shell := os.Getenv("ComSpec")
	if shell == "" {
		shell = "cmd.exe"
	}
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `/d /s /c "` + strings.Join(parts, " ") + `"`}
	return cmd
}
