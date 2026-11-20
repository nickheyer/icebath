//go:build windows

package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Command running the line through ComSpec /c with a hidden window like subprocess shell mode on Windows
func pyShellCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		if !filepath.IsAbs(comspec) {
			return nil, errors.New("shell not found: neither %ComSpec% nor %SystemRoot% is set")
		}
	}
	cmd := exec.CommandContext(ctx, comspec)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: fmt.Sprintf(`%s /c "%s"`, comspec, command), HideWindow: true}
	return cmd, nil
}
