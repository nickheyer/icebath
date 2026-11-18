//go:build windows

package photon

import (
	"os"
	"os/exec"
	"syscall"
)

// Orphan reaping relies on lsof and ps, which Windows hosts lack
const reapSupported = false

// Terminates a pid, Windows having no gentler signal
func signalPID(pid int, kill bool) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// Runs the child headless so no console window opens
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
}

// Terminates the sidecar process
func terminateTree(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
