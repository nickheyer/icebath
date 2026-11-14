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

func (e *Engine) acquireRepairLock(ctx context.Context, lockPath string, f *os.File, timeout time.Duration) (lockAcquire, *os.File) {
	deadline := time.Now().Add(timeout)
	for {
		err := fileutil.TryLockFile(f, fileutil.Range(0, 1))
		if err == nil {
			return lockHeld, f
		}
		if !errors.Is(err, fileutil.ErrLocked) {
			slog.Warn(fmt.Sprintf("Could not acquire state.db repair lock %s (%v) — skipping schema surgery on a "+
				"non-contention error.", lockPath, err))
			return lockFailed, f
		}
		if !time.Now().Before(deadline) {
			return lockBusy, f
		}
		if sleepCtx(ctx, repairLockPoll) != nil {
			return lockAbandoned, f
		}
	}
}
