//go:build linux || darwin

package state

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// fcntl record lock through the platform struct flock layout
func unixFcntlFlock(fd, cmd int, lockType int16, start, length int64) error {
	lk := unix.Flock_t{Type: lockType, Whence: 0, Start: start, Len: length}
	return unix.FcntlFlock(uintptr(fd), cmd, &lk)
}

// OFD guard on Linux (F_OFD_SETLK 37) and XNU (90)
func platformLockGuard() *lockGuardOps {
	ofd := 37
	if runtime.GOOS == "darwin" {
		ofd = 90
	}
	return newLockGuardOps(&ofd, unix.F_SETLK, unix.F_RDLCK, unix.F_UNLCK, unixFcntlFlock)
}
