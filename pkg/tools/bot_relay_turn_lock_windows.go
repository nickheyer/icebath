//go:build windows

package tools

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"time"

	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
	"golang.org/x/sys/windows"
)

// Non blocking byte range lock probe with short sleeps up to the budget, a TurnBusyError when exhausted
func acquireTurnLock(ctx context.Context, root, profile string, timeoutSeconds *float64) (func(), error) {
	var budget float64
	if timeoutSeconds == nil {
		budget = TurnWaitSeconds(ctx)
	} else {
		budget = math.Max(0, *timeoutSeconds)
	}
	path := TurnLockPath(root, profile)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, botRelayTurnLockFileMode)
	if err != nil {
		return nil, err
	}
	handle := windows.Handle(f.Fd())
	start := timeutil.Monotonic()
	deadline := start + budget
	for {
		var overlapped windows.Overlapped
		lockErr := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if lockErr == nil {
			break
		}
		if !errors.Is(lockErr, windows.ERROR_LOCK_VIOLATION) && !errors.Is(lockErr, windows.ERROR_IO_PENDING) {
			_ = f.Close()
			return nil, &os.PathError{Op: "lock", Path: path, Err: lockErr}
		}
		now := timeutil.Monotonic()
		if now >= deadline {
			_ = f.Close()
			return nil, &TurnBusyError{Profile: profile, WaitedSeconds: now - start}
		}
		time.Sleep(time.Duration(math.Min(botRelayTurnLockSleepMax, math.Max(botRelayTurnLockSleepMin, deadline-now)) * float64(time.Second)))
	}
	return func() {
		var overlapped windows.Overlapped
		_ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
		_ = f.Close()
	}, nil
}
