//go:build !windows

package skills

import (
	"errors"
	"os"
	"syscall"
)

// Opens the lock file for appending, creating it when missing
func openSkillLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
}

// Takes an exclusive flock on the handle and returns its release
func lockSkillFile(f *os.File) (func(), error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EINTR) {
			return nil, err
		}
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
