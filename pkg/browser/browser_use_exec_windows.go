//go:build windows

package browser

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// Kills the CLI's process tree through taskkill, the only group wide kill Windows offers
func killCLIProcessGroup(pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = c.Run()
}

// The harness daemon runs outside this process's job on Windows, so it is never owned here
func ownHarnessDaemon(context.Context, map[string]string) daemonOwnership { return ownNever }
