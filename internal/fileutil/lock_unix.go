//go:build unix

package fileutil

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// One non blocking flock on f, contention mapped to ErrLocked
func kernelLock(f *os.File, cfg lockConfig) error {
	err := flock(f, lockHow(cfg)|unix.LOCK_NB)
	if err != nil && lockContention(err) {
		return &fs.PathError{Op: "flock", Path: f.Name(), Err: ErrLocked}
	}
	return err
}

// Waits in the kernel for as long as another holder keeps the flock
func blockingLock(f *os.File, cfg lockConfig) error { return flock(f, lockHow(cfg)) }

// Shared or exclusive flock operation the settings ask for
func lockHow(cfg lockConfig) int {
	if cfg.shared {
		return unix.LOCK_SH
	}
	return unix.LOCK_EX
}

// Drops the flock f holds
func unlockFile(f *os.File, _ lockConfig) error { return flock(f, unix.LOCK_UN) }

// Applies one BSD advisory operation to the descriptor of f until it stops being interrupted
func flock(f *os.File, how int) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	ctlErr := conn.Control(func(fd uintptr) {
		for {
			opErr = unix.Flock(int(fd), how)
			if !errors.Is(opErr, unix.EINTR) {
				return
			}
		}
	})
	if ctlErr != nil {
		return ctlErr
	}
	if opErr != nil {
		return &fs.PathError{Op: "flock", Path: f.Name(), Err: opErr}
	}
	return nil
}

// Reports whether a flock failure means another holder rather than a filesystem that cannot lock
func lockContention(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EDEADLK)
}

// Lock file opened read write and created with the configured mode, O_NOFOLLOW refusing a symlink
func openLockFile(path string, cfg lockConfig) (*os.File, error) {
	flags := os.O_RDWR | os.O_CREATE
	if cfg.noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	return os.OpenFile(path, flags, cfg.perm)
}

// Reports whether a process with pid exists, one this process may not signal still counting as alive
func processAlive(pid int) bool {
	return !errors.Is(unix.Kill(pid, 0), unix.ESRCH)
}
