package gateway

import (
	"fmt"
	"io/fs"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// The scoped lock directory this token cannot create, read or replace locks in, a permission error
type ScopedLockDirUnusable struct {
	Msg   string
	Dir   string
	Cause error
}

func (e *ScopedLockDirUnusable) Error() string {
	return e.Msg
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
		"is repaired. To repair, %s", lockDir, cause, home.UnusableDirRemedy(lockDir))
	if warned {
		pkgLog().Debug(msg)
	} else {
		pkgLog().Warn(msg)
	}
	return &ScopedLockDirUnusable{Msg: fmt.Sprintf("gateway lock directory is not usable: %v", cause), Dir: lockDir, Cause: cause}
}

// Forgets the report of lockDir once a lock was taken in it
func unusableLockDirRecovered(lockDir string) {
	unusableLockDirMu.Lock()
	defer unusableLockDirMu.Unlock()
	delete(unusableLockDirWarned, lockDir)
}
