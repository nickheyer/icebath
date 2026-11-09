//go:build !windows

package selfupdate

import (
	"errors"
	"os"
	"syscall"
)

// flock's EWOULDBLOCK and EACCES or EDEADLK, another process holds the claim
func claimHeldErrno(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK || errno == syscall.EACCES || errno == syscall.EDEADLK
}

// Takes or drops a non blocking exclusive lock on the whole file, false only while another launch holds it
func lockClaimFile(f *os.File, lock bool) bool {
	how := syscall.LOCK_UN
	if lock {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		return !(lock && claimHeldErrno(err))
	}
	return true
}

// Lock attempt of the ZIP swap owner lock, held true for another live owner and the error for a lock that cannot be taken
func tryZipSwapLock(f *os.File) (held bool, err error) {
	lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if lockErr == nil {
		return false, nil
	}
	if claimHeldErrno(lockErr) {
		return true, nil
	}
	return false, lockErr
}

// Device and inode of an entry as its lstat reports them, false when the platform has none
func fileIdentity(_ string, info os.FileInfo) (dev, ino uint64, ok bool) {
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

// Modification time of an entry in nanoseconds
func mtimeNanos(info os.FileInfo) int64 { return info.ModTime().UnixNano() }

// Link count of an entry, one when the platform does not report it
func linkCountOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
