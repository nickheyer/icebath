//go:build windows

package provider

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// KillProcessTree terminates a process and its descendants with taskkill, the signal is ignored
func KillProcessTree(pid int, sig syscall.Signal) bool {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err != nil {
			slog.Debug(fmt.Sprintf("kill_process_tree: taskkill failed for pid %d: %v", pid, err))
			return false
		}
		return true
	case <-time.After(15 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		slog.Debug(fmt.Sprintf("kill_process_tree: taskkill timed out for pid %d", pid))
		return false
	}
}
