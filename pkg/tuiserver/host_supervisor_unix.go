//go:build !windows

package tuiserver

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Whether pid names a process, a permission refusal counting as alive
func hostPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Sends SIGTERM or SIGKILL, false when the pid is gone or the signal failed
func hostSignalPID(pid int, kill bool, label string) bool {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	err := syscall.Kill(pid, sig)
	if err == nil {
		return true
	}
	if !errors.Is(err, syscall.ESRCH) {
		logger().Debug(fmt.Sprintf("failed to %s compute host pid=%d: %s", label, pid, err))
	}
	return false
}

// Process attributes that start the child in a session of its own
func hostNewSessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Command line of a pid from procfs, empty when unreadable
func hostProcCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return string(data)
}

// One write of data to path opened for appending, the kernel O_APPEND keeping each record whole
func hostAppendWrite(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
