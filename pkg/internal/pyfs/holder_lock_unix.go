//go:build !windows

package pyfs

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Released lock files stay empty since flock needs no byte to lock
const holderReleasePad = ""

// Opens the lock file in append mode, creating it, as open a+ does
func openHolderLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
}

// Non blocking exclusive flock, or its release
func holderKernelLock(f *os.File, acquire bool) error {
	how := unix.LOCK_UN
	if acquire {
		how = unix.LOCK_EX | unix.LOCK_NB
	}
	return unix.Flock(int(f.Fd()), how)
}

// Reports whether a flock failure means another holder rather than a broken filesystem
func isHolderLockContention(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EDEADLK)
}

// Reports whether no process with the pid exists, a pid that cannot be signalled still counting as alive
func holderGone(pid int) bool {
	return errors.Is(unix.Kill(pid, 0), unix.ESRCH)
}
