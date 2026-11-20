//go:build !windows

package verify

import (
	"errors"
	"syscall"
)

// Process group of a pid, nil where the platform has none
var getpgid = syscall.Getpgid

// Signals a whole process group with SIGTERM or SIGKILL
var killpg = func(pgid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pgid, sig)
}

// ProcessLookupError or PermissionError of a signal or group lookup
func isLookupOrPermission(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// Popen.send_signal, nothing once the child has exited or vanished
func (c *childProcess) signal(sig syscall.Signal) error {
	if c.poll() {
		return nil
	}
	if err := syscall.Kill(c.pid(), sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (c *childProcess) terminate() error { return c.signal(syscall.SIGTERM) }
func (c *childProcess) kill() error      { return c.signal(syscall.SIGKILL) }
