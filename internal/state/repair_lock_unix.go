//go:build unix

package state

import (
	"context"
	"os"
	"time"
)

func (e *Engine) acquireRepairLock(ctx context.Context, lockPath string, f *os.File, timeout time.Duration) (lockAcquire, *os.File) {
	return e.acquireRebuildLock(ctx, "state.db repair lock", lockPath, f, timeout)
}
