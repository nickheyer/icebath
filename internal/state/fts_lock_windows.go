package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Bounded acquire of the one byte range lock Windows rebuilds serialize on, with no orphan break
func (e *Engine) acquireRebuildLock(ctx context.Context, what, lockPath string, f *os.File, timeout time.Duration) (lockAcquire, *os.File) {
	deadline := time.Now().Add(timeout)
	for {
		err := fileutil.TryLockFile(f, fileutil.Range(0, 1))
		if err == nil {
			return lockHeld, f
		}
		if !errors.Is(err, fileutil.ErrLocked) {
			slog.Warn(fmt.Sprintf("Could not acquire %s %s (%v) — deferring on a non-contention error.", what, lockPath, err))
			return lockFailed, f
		}
		if !time.Now().Before(deadline) {
			return lockBusy, f
		}
		if sleepCtx(ctx, ftsRebuildLockPoll) != nil {
			return lockAbandoned, f
		}
	}
}

// Windows rebuild locks carry no holder record
func readHolderRecord(*os.File) lockHolderRecord { return lockHolderRecord{} }

// Lets go of the byte range lock
func releaseRebuildLock(f *os.File) { _ = fileutil.UnlockFile(f, fileutil.Range(0, 1)) }
