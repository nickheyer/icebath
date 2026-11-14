package state

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Admission waits of a startup rebuild, the poll between attempts, and the re-acquire after an orphan break
const (
	ftsRebuildLockTimeout = 120 * time.Second
	ftsRebuildLockPoll    = 100 * time.Millisecond
	lockBreakReacquire    = 5 * time.Second
)

// Outcome of one bounded lock acquire, a non-contention failure already logged
type lockAcquire int

// Held, held elsewhere past the deadline, refused by an error polling cannot fix, or abandoned when the context ended
const (
	lockHeld lockAcquire = iota
	lockBusy
	lockFailed
	lockAbandoned
)

// Holder a rebuild records in its lock file so a waiter can tell an orphan, nil for absent fields
type lockHolderRecord struct {
	pid        *int64
	pidns      *string
	startTicks *int64
	acquiredAt *float64
}

// Holds the cross-process full rebuild authority of dbPath, a release func and false when it was not admitted
func (e *Engine) ftsRebuildAdmission(ctx context.Context, dbPath string, timeout time.Duration) (func(), bool) {
	lockPath := dbPath + ".fts_rebuild.lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not open FTS rebuild lock %s (%v) — deferring this rebuild "+
			"rather than running it without cross-process authority.", lockPath, err))
		return func() {}, false
	}
	outcome, f := e.acquireRebuildLock(ctx, "FTS rebuild lock", lockPath, f, max(timeout, 0))
	if outcome != lockHeld {
		if outcome == lockBusy {
			holder := describeLockHolder(readHolderRecord(f))
			if timeout <= 0 {
				slog.Info(fmt.Sprintf("FTS rebuild lock %s is busy — deferring this retry (the stale-FTS breadcrumb "+
					"keeps it retryable). Recorded holder: %s.", lockPath, holder))
			} else {
				slog.Warn(fmt.Sprintf("FTS rebuild lock %s held by another process for more than %.0fs — deferring "+
					"this rebuild to avoid racing the holder (the stale-FTS breadcrumb keeps it retryable). Recorded "+
					"holder: %s.", lockPath, timeout.Seconds(), holder))
			}
		}
		if outcome == lockAbandoned {
			slog.Warn(fmt.Sprintf("Stopped waiting for FTS rebuild lock %s before its %.0fs bound (%v) — deferring this rebuild.",
				lockPath, timeout.Seconds(), context.Cause(ctx)))
		}
		_ = f.Close()
		return func() {}, false
	}
	return func() {
		releaseRebuildLock(f)
		_ = f.Close()
	}, true
}

// Holder identity for a deferral warning
func describeLockHolder(r lockHolderRecord) string {
	if r.pid == nil {
		return "unknown (no holder record; pre-fix writer or non-icebath)"
	}
	age := ""
	if r.acquiredAt != nil {
		age = fmt.Sprintf(", acquired %.0fs ago", float64(time.Now().UnixNano())/1e9-*r.acquiredAt)
	}
	return fmt.Sprintf("pid %d%s", *r.pid, age)
}
