//go:build !windows

package install

import (
	"context"
	"os/exec"
	"runtime"
	"syscall"
)

// Puts the child in its own session so it outlives the installer
func detachCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Standard handles need no preparation where spawns inherit only the stdio they are given
func prepareDetachedSpawn() {}

// Child streams of an update run start with no console window to hide
func hideConsole(cmd *exec.Cmd) {}

// Clears quarantine on a placed macOS desktop app, nothing to finish on Linux
func (r *Runner) finishDesktopPlatform(ctx context.Context, exe string) {
	if runtime.GOOS != "darwin" {
		return
	}
	if app := appBundleForExe(exe); app != "" {
		_ = exec.CommandContext(ctx, "/usr/bin/xattr", "-dr", "com.apple.quarantine", app).Run()
	}
}

// No shortcuts exist outside Windows
func shortcutPaths() []string { return nil }
