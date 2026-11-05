//go:build !windows

package manage

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Reports whether a missing lock or one a read only descriptor can flock has no holder
func lockUnheldReadOnly(path string) (bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd)
	return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil, nil
}
