//go:build windows

package googlemeet

import (
	"errors"
	"os"
	"syscall"
)

// Process attributes that start the bot in its own process group
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Terminates pid for either signal like os.kill on Windows, a process already gone being no error
func killPID(pid int, _ syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
