package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"syscall"
)

// The scoped lock directory this token cannot create, read or replace locks in, a permission error
type ScopedLockDirUnusable struct {
	Errno syscall.Errno
	Msg   string
	Dir   string
	Cause error
}

func (e *ScopedLockDirUnusable) Error() string {
	return fmt.Sprintf("[Errno %d] %s: '%s'", int(e.Errno), e.Msg, e.Dir)
}

func (e *ScopedLockDirUnusable) Unwrap() error { return e.Cause }

func (e *ScopedLockDirUnusable) Is(target error) bool { return target == fs.ErrPermission }

var (
	unusableLockDirMu     sync.Mutex
	unusableLockDirWarned = map[string]bool{}
)

// Logs lockDir as unusable, a warning once and debug after, and returns the error to raise
func unusableLockDir(lockDir string, cause error) error {
	unusableLockDirMu.Lock()
	warned := unusableLockDirWarned[lockDir]
	unusableLockDirWarned[lockDir] = true
	unusableLockDirMu.Unlock()
	msg := fmt.Sprintf("Gateway lock directory %s is not usable by this process (%v); it was likely created "+
		"by an elevated (administrator) Icebath. Platforms that need a token lock stay down until it "+
		"is repaired. To repair, %s", lockDir, cause, unusableDirRemedy(lockDir))
	if warned {
		slog.Debug(msg)
	} else {
		slog.Warn(msg)
	}
	var errno syscall.Errno
	errors.As(cause, &errno)
	return &ScopedLockDirUnusable{Errno: errno, Msg: fmt.Sprintf("gateway lock directory is not usable: %v", cause), Dir: lockDir, Cause: cause}
}

// Forgets the report of lockDir once a lock was taken in it
func unusableLockDirRecovered(lockDir string) {
	unusableLockDirMu.Lock()
	defer unusableLockDirMu.Unlock()
	delete(unusableLockDirWarned, lockDir)
}

// Forgets every unusable lock directory report
func ResetUnusableLockDirReports() {
	unusableLockDirMu.Lock()
	defer unusableLockDirMu.Unlock()
	clear(unusableLockDirWarned)
}

// Whether lockDir was reported unusable by this process
func UnusableLockDirReported(lockDir string) bool {
	unusableLockDirMu.Lock()
	defer unusableLockDirMu.Unlock()
	return unusableLockDirWarned[lockDir]
}
