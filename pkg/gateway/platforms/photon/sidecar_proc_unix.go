//go:build !windows

package photon

import (
	"os/exec"
	"syscall"
)

// Orphaned sidecars are found through lsof and ps on POSIX hosts
const reapSupported = true

// Sends SIGTERM, or SIGKILL when kill is set, to a pid
func signalPID(pid int, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(pid, sig)
}

// Console windows only exist on Windows
func hideWindow(cmd *exec.Cmd) {}

// SIGTERM to the sidecar's process group, the process alone when its group is gone
func terminateTree(cmd *exec.Cmd) {
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		if syscall.Kill(-pgid, syscall.SIGTERM) == nil {
			return
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
}
