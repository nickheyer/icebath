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
func (e *Engine) acquireRebuildLock(ctx context.Context, lockPath string, f *os.File, timeout time.Duration) (lockAcquire, *os.File) {
	deadline := time.Now().Add(timeout)
	for {
		err := fileutil.TryLockFile(f, fileutil.Range(0, 1))
		if err == nil {
			return lockHeld, f
		}
		if !errors.Is(err, fileutil.ErrLocked) {
			slog.Warn(fmt.Sprintf("Could not acquire FTS rebuild lock %s (%v) — deferring on a non-contention error.", lockPath, err))
			return lockFailed, f
		}
		if !time.Now().Before(deadline) {
			return lockBusy, f
		}
		if sleepCtx(ctx, ftsRebuildLockPoll) != nil {
			return lockBusy, f
		}
	}
}

// Windows rebuild locks carry no holder record
func readHolderRecordIfRecorded(*os.File) lockHolderRecord { return lockHolderRecord{} }

// Lets go of the byte range lock
func releaseRebuildLock(f *os.File) { _ = fileutil.UnlockFile(f, fileutil.Range(0, 1)) }
