//go:build windows

package verify

import (
	"errors"
	"syscall"
)

// Windows has no process groups to look up or signal
var (
	getpgid func(pid int) (int, error)
	killpg  func(pgid int, force bool) error
)

// ProcessLookupError or PermissionError of a terminate call
func isLookupOrPermission(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, syscall.EPERM)
}

// Popen.terminate, TerminateProcess unless the child already exited
func (c *childProcess) terminate() error {
	if c.poll() {
		return nil
	}
	if err := c.cmd.Process.Kill(); err != nil && !c.poll() {
		return err
	}
	return nil
}

func (c *childProcess) kill() error { return c.terminate() }
