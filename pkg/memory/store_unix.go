//go:build !windows

package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
)

// Python labels OS errors on POSIX hosts with the errno number
const errnoLabel = "Errno"

// Exclusive flock on the owner only lock sibling, refusing a symlinked lock path
func fileLock(ctx context.Context, path string) (func(), error) {
	lockPath := path + ".lock"
	if _, err := home.Mkdir(ctx, filepath.Dir(lockPath)); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: lockPath, Err: err}
	}
	if err := syscall.Fchmod(fd, 0o600); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), lockPath)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(fd, syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Text mode writes keep LF on POSIX hosts
func platformNewlines(s string) string { return s }
