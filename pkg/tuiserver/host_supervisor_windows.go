//go:build windows

package tuiserver

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Whether pid names a live process
func hostPIDAlive(pid int) bool { return pid > 0 && proc.Exists(pid) }

// Terminates the process, the TerminateProcess both SIGTERM and SIGKILL map to, false when it is gone or the call failed
func hostSignalPID(pid int, kill bool, label string) bool {
	p, err := os.FindProcess(pid)
	if err == nil {
		err = p.Kill()
	}
	if err == nil {
		return true
	}
	if !errors.Is(err, os.ErrProcessDone) {
		logger().Debug(fmt.Sprintf("failed to %s compute host pid=%d: %s", label, pid, err))
	}
	return false
}

// Process attributes of the child, a new session being a POSIX only spawn option
func hostNewSessionAttr() *syscall.SysProcAttr { return nil }

// Command line of a pid, procfs being absent here
func hostProcCmdline(pid int) string { return "" }
