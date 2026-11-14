//go:build unix

package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/proc"
)

// Bounded flock acquire that breaks a lock an orphaned descriptor of a provably dead holder keeps, re-checking the inode
func (e *Engine) acquireRebuildLock(ctx context.Context, what, lockPath string, f *os.File, timeout time.Duration) (lockAcquire, *os.File) {
	deadline := time.Now().Add(timeout)
	broke := false
	for {
		if err := fileutil.TryLockFile(f); err != nil {
			if !errors.Is(err, fileutil.ErrLocked) {
				slog.Warn(fmt.Sprintf("Could not acquire %s %s (%v) — deferring rather than waiting out "+
					"the %.0fs holder timeout on a non-contention error.", what, lockPath, err, timeout.Seconds()))
				return lockFailed, f
			}
			if time.Now().Before(deadline) {
				if sleepCtx(ctx, ftsRebuildLockPoll) != nil {
					return lockAbandoned, f
				}
				continue
			}
			if broke {
				return lockBusy, f
			}
			record := readHolderRecord(f)
			if !e.lockHolderProvablyDead(record) {
				return lockBusy, f
			}
			slog.Warn(fmt.Sprintf("%s %s is held by an orphaned file descriptor (recorded holder pid %d is "+
				"dead — a forked child inherited the lock fd); breaking the stale lock and retaking it on a fresh file.",
				what, lockPath, *record.pid))
			if err := os.Remove(lockPath); err != nil {
				slog.Warn(fmt.Sprintf("Could not break stale %s %s (%v) — deferring.", what, lockPath, err))
				return lockBusy, f
			}
			_ = f.Close()
			fresh, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o666)
			if err != nil {
				slog.Warn(fmt.Sprintf("Could not break stale %s %s (%v) — deferring.", what, lockPath, err))
				return lockBusy, f
			}
			f, broke, deadline = fresh, true, time.Now().Add(lockBreakReacquire)
			continue
		}
		held, herr := f.Stat()
		named, nerr := os.Stat(lockPath)
		if herr == nil && nerr == nil && os.SameFile(held, named) {
			e.writeHolderRecord(f)
			return lockHeld, f
		}
		_ = f.Close()
		fresh, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o666)
		if err != nil {
			return lockBusy, f
		}
		f = fresh
		if !time.Now().Before(deadline) {
			return lockBusy, f
		}
	}
}

// Reports a recorded holder that is provably dead or whose pid was recycled, anything indeterminate deferring
func (e *Engine) lockHolderProvablyDead(r lockHolderRecord) bool {
	if r.pid == nil || *r.pid <= 0 {
		return false
	}
	ns := ""
	if r.pidns != nil {
		ns = *r.pidns
	}
	if !e.persistentRecordCheckable(ns) {
		return false
	}
	pid := int(*r.pid)
	if !proc.Exists(pid) {
		return true
	}
	if r.startTicks == nil {
		return false
	}
	current, err := proc.Fingerprint(pid)
	return err == nil && current != *r.startTicks
}

// Stamps this process as the holder under the lock so a timed out waiter can judge it
func (e *Engine) writeHolderRecord(f *os.File) {
	pid := os.Getpid()
	record := map[string]any{"pid": pid, "pidns": nil, "start_ticks": nil, "acquired_at": float64(time.Now().UnixNano()) / 1e9}
	if id, _ := e.pidNamespace(); id != "" {
		record["pidns"] = id
	}
	if ticks, err := proc.Fingerprint(pid); err == nil {
		record["start_ticks"] = ticks
	}
	payload, err := fileutil.EncodeJSON(record, fileutil.JSONStyle{Sorted: true, ASCII: true})
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not encode the FTS rebuild lock holder record: %v", err))
	}
	rewriteLockFile(f, payload)
}

// Erases the holder record and lets go of the lock, a surviving record marking an abnormal exit
func releaseRebuildLock(f *os.File) {
	rewriteLockFile(f, nil)
	_ = fileutil.UnlockFile(f)
}

// Decodes one field of a holder record into v, false when the field is absent, null or of another type
func holderField(fields map[string]json.RawMessage, name string, v any) bool {
	raw, ok := fields[name]
	return ok && string(bytes.TrimSpace(raw)) != "null" && json.Unmarshal(raw, v) == nil
}

// Holder record in the lock file, every field nil when the file is empty or holds no JSON object
func readHolderRecord(f *os.File) lockHolderRecord {
	buf := make([]byte, 4096)
	n, _ := f.ReadAt(buf, 0)
	var fields map[string]json.RawMessage
	if n == 0 || json.Unmarshal(buf[:n], &fields) != nil {
		return lockHolderRecord{}
	}
	var r lockHolderRecord
	var pid json.Number
	if holderField(fields, "pid", &pid) {
		if i, err := pid.Int64(); err == nil {
			r.pid = &i
		} else if fl, err := pid.Float64(); err == nil {
			i := int64(fl)
			r.pid = &i
		}
	}
	var ns string
	if holderField(fields, "pidns", &ns) {
		r.pidns = &ns
	}
	var ticks int64
	if holderField(fields, "start_ticks", &ticks) {
		r.startTicks = &ticks
	}
	var at float64
	if holderField(fields, "acquired_at", &at) {
		r.acquiredAt = &at
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
