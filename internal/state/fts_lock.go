package state

import (
	"context"
	"encoding/json"
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

// Held, held elsewhere past the deadline, or refused by an error polling cannot fix
const (
	lockHeld lockAcquire = iota
	lockBusy
	lockFailed
)

// Holder record a rebuild writes into its lock file so a waiter can tell an orphan from a live holder
type lockHolderRecord struct {
	PID        *int64   `json:"pid"`
	PIDNS      *string  `json:"pidns"`
	StartTicks *int64   `json:"start_ticks"`
	AcquiredAt *float64 `json:"acquired_at"`
	present    bool
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
	outcome, f := e.acquireRebuildLock(ctx, lockPath, f, max(timeout, 0))
	if outcome != lockHeld {
		if outcome == lockBusy {
			holder := describeLockHolder(readHolderRecordIfRecorded(f))
			if timeout <= 0 {
				slog.Info(fmt.Sprintf("FTS rebuild lock %s is busy — deferring this retry (the stale-FTS breadcrumb "+
					"keeps it retryable). Recorded holder: %s.", lockPath, holder))
			} else {
				slog.Warn(fmt.Sprintf("FTS rebuild lock %s held by another process for more than %.0fs — deferring "+
					"this rebuild to avoid racing the holder (the stale-FTS breadcrumb keeps it retryable). Recorded "+
					"holder: %s.", lockPath, timeout.Seconds(), holder))
			}
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
	if !r.present || r.PID == nil {
		return "unknown (no holder record; pre-fix writer or non-icebath)"
	}
	age := ""
	if r.AcquiredAt != nil {
		age = fmt.Sprintf(", acquired %.0fs ago", float64(time.Now().UnixNano())/1e9-*r.AcquiredAt)
	}
	return fmt.Sprintf("pid %d%s", *r.PID, age)
}

// Holder record in the lock file, absent when the file is empty or holds no JSON object
func readHolderRecord(f *os.File) lockHolderRecord {
	buf := make([]byte, 4096)
	n, _ := f.ReadAt(buf, 0)
	if n == 0 {
		return lockHolderRecord{}
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(buf[:n], &probe) != nil {
		return lockHolderRecord{}
	}
	var r lockHolderRecord
	r.present = true
	if raw, ok := probe["pid"]; ok {
		var v json.Number
		if json.Unmarshal(raw, &v) == nil {
			if i, err := v.Int64(); err == nil {
				r.PID = &i
			} else if fl, err := v.Float64(); err == nil {
				i := int64(fl)
				r.PID = &i
			}
		}
	}
	if raw, ok := probe["pidns"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			r.PIDNS = &v
		}
	}
	if raw, ok := probe["start_ticks"]; ok {
		var v int64
		if json.Unmarshal(raw, &v) == nil {
			r.StartTicks = &v
		}
	}
	if raw, ok := probe["acquired_at"]; ok {
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			r.AcquiredAt = &v
		}
	}
	return r
}

// Rewrites the lock file to payload at offset zero, a failure leaving the record the next holder overwrites
func rewriteLockFile(f *os.File, payload []byte) {
	if err := f.Truncate(0); err != nil {
		return
	}
	if len(payload) > 0 {
		_, _ = f.WriteAt(payload, 0)
	}
}
