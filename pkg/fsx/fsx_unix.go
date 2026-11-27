//go:build !windows

package fsx

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// A lock file is opened refusing a link planted in its place
const noFollow = unix.O_NOFOLLOW

// One attempt at the exclusive lock without waiting, false and no error when another handle holds it
func tryLock(f *os.File) (bool, error) {
	err := flock(f, unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK), errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EACCES):
		return false, nil
	}
	return false, err
}

// Releases the lock f holds
func unlock(f *os.File) error { return flock(f, unix.LOCK_UN) }

func flock(f *os.File, how int) error {
	for {
		err := unix.Flock(int(f.Fd()), how)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EINTR) {
			return &fs.PathError{Op: "flock", Path: f.Name(), Err: err}
		}
	}
}

// Whether no process with the pid exists, one this process may not signal still counting as alive
func processGone(pid int) bool { return errors.Is(unix.Kill(pid, 0), unix.ESRCH) }

// Whether a failure is another program holding the file for a while
func Held(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EBUSY)
}

// Whether the path is a regular file this process may run, through links
func Executable(path string) bool { return IsFile(path) && unix.Access(path, unix.X_OK) == nil }

// Flushes a folder's entries, so a rename in it outlives a crash
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
