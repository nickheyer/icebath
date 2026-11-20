//go:build !windows

package plugins

import (
	"errors"
	"os"
	"syscall"
)

// Takes an exclusive flock on the handle and returns its release
func lockStateFile(handle *os.File) (func(), error) {
	for {
		err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EINTR) {
			return nil, err
		}
	}
	return func() { _ = syscall.Flock(int(handle.Fd()), syscall.LOCK_UN) }, nil
}
