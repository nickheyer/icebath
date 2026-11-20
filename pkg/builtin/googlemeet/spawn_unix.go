//go:build !windows

package googlemeet

import (
	"errors"
	"os"
	"syscall"
)

// Process attributes that start the bot in its own session
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Sends sig to pid, a process already gone being no error
func killPID(pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
