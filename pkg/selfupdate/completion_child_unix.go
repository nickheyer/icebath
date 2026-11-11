//go:build !windows

package selfupdate

import (
	"os"
	"syscall"
)

// New session for the completion child, which inherits the locked checkout descriptors
func completionChildAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Kills the process group of the completion child, an already gone group counting as killed
func killCompletionTree(process *os.Process) error {
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
