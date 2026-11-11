//go:build !windows

package tools

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Non blocking flock probe with short sleeps up to the budget, a TurnBusyError when exhausted
func acquireTurnLock(ctx context.Context, root, profile string, timeoutSeconds *float64) (func(), error) {
	var budget float64
	if timeoutSeconds == nil {
		configured, err := TurnWaitSeconds(ctx)
		if err != nil {
			return nil, err
		}
		budget = configured
	} else {
		budget = math.Max(0, *timeoutSeconds)
	}
	path := TurnLockPath(root, profile)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC, botRelayTurnLockFileMode)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	start := pythread.Monotonic()
	deadline := start + budget
	for {
		if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			break
		}
		now := pythread.Monotonic()
		if now >= deadline {
			_ = syscall.Close(fd)
			return nil, &TurnBusyError{Profile: profile, WaitedSeconds: now - start}
		}
		time.Sleep(time.Duration(math.Min(botRelayTurnLockSleepMax, math.Max(botRelayTurnLockSleepMin, deadline-now)) * float64(time.Second)))
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = syscall.Close(fd)
	}, nil
}
