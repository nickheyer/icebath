//go:build !windows

package pyfs

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// One non blocking exclusive flock attempt, false without error when another handle holds it
func TryLockFile(f *os.File) (bool, error) {
	err := flock(f, unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}
