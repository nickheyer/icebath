//go:build !windows

package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

const (
	oNoFollow = syscall.O_NOFOLLOW
	oNonblock = syscall.O_NONBLOCK
)

// Taskkill exists only on Windows hosts
var statusTaskkill = func(pid int) (int, string, string, error) {
	return 0, "", "", fmt.Errorf("taskkill: %w", fs.ErrNotExist)
}

// Takes the exclusive nonblocking flock of f, false when another description holds it
func tryAcquireFileLock(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

func releaseFileLock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// Sends SIGTERM or SIGKILL, a vanished pid wrapping os.ErrProcessDone like ProcessLookupError
func killPid(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	err := syscall.Kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("%w: %w", os.ErrProcessDone, err)
	}
	if err != nil {
		return &os.SyscallError{Syscall: "kill", Err: err}
	}
	return nil
}

// Refuses a lock inode that is not a regular file owned by this user and narrows a group or world readable one to 0600
func tightenLockMode(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(st.Uid) != os.Getuid() {
		return &fs.PathError{Op: "open", Path: f.Name(), Err: fmt.Errorf("unsafe gateway lock owner or type: %w", fs.ErrPermission)}
	}
	if info.Mode().Perm()&0o077 != 0 {
		if st.Nlink != 1 {
			return &fs.PathError{Op: "open", Path: f.Name(), Err: fmt.Errorf("unsafe gateway lock link count: %w", fs.ErrPermission)}
		}
		return f.Chmod(0o600)
	}
	return nil
}

func sameInode(a, b os.FileInfo) bool {
	sa, okA := a.Sys().(*syscall.Stat_t)
	sb, okB := b.Sys().(*syscall.Stat_t)
	return okA && okB && sa.Dev == sb.Dev && sa.Ino == sb.Ino
}

// Unlinks another user's unwritable gateway.lock once a read only flock proves it unheld
func unlinkStaleLock(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || st.Nlink != 1 {
		return false, nil
	}
	if _, live := liveFromRecord(readPidRecord(pathWithName(path, "gateway.pid"))); live {
		return false, nil
	}
	probe, err := os.OpenFile(path, os.O_RDONLY|oNoFollow|oNonblock, 0)
	if errors.Is(err, fs.ErrPermission) {
		return false, &fs.PathError{Op: "open", Path: path, Err: fmt.Errorf("%s: %w", ForeignLockRecovery(path), fs.ErrPermission)}
	}
	if err != nil {
		return false, err
	}
	defer probe.Close()
	pinfo, err := probe.Stat()
	if err != nil {
		return false, err
	}
	if !sameInode(pinfo, info) {
		return false, nil
	}
	if syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return false, nil
	}
	return unlinkIfSame(path, info)
}

func unlinkIfSame(path string, info os.FileInfo) (bool, error) {
	now, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !sameInode(now, info) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

// Whether path still names the inode f locked
func stillNames(path string, f *os.File) bool {
	now, err := os.Lstat(path)
	if err != nil {
		return false
	}
	held, err := f.Stat()
	return err == nil && sameInode(now, held)
}
