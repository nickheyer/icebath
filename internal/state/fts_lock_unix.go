//go:build unix

package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/proc"
)

// Bounded flock acquire that breaks a lock an orphaned descriptor of a provably dead holder keeps, re-checking the inode
func (e *Engine) acquireRebuildLock(ctx context.Context, lockPath string, f *os.File, timeout time.Duration) (lockAcquire, *os.File) {
	deadline := time.Now().Add(timeout)
	broke := false
	for {
		if err := fileutil.TryLockFile(f); err != nil {
			if !errors.Is(err, fileutil.ErrLocked) {
				slog.Warn(fmt.Sprintf("Could not acquire FTS rebuild lock %s (%v) — deferring rather than waiting out "+
					"the %.0fs holder timeout on a non-contention error.", lockPath, err, timeout.Seconds()))
				return lockFailed, f
			}
			if time.Now().Before(deadline) {
				if sleepCtx(ctx, ftsRebuildLockPoll) != nil {
					return lockBusy, f
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
			pid := "None"
			if record.PID != nil {
				pid = fmt.Sprint(*record.PID)
			}
			slog.Warn(fmt.Sprintf("FTS rebuild lock %s is held by an orphaned file descriptor (recorded holder pid %s is "+
				"dead — a forked child inherited the lock fd); breaking the stale lock and retaking it on a fresh file.",
				lockPath, pid))
			if err := os.Remove(lockPath); err != nil {
				slog.Warn(fmt.Sprintf("Could not break stale FTS rebuild lock %s (%v) — deferring.", lockPath, err))
				return lockBusy, f
			}
			_ = f.Close()
			fresh, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o666)
			if err != nil {
				slog.Warn(fmt.Sprintf("Could not break stale FTS rebuild lock %s (%v) — deferring.", lockPath, err))
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
	if r.PID == nil || *r.PID <= 0 {
		return false
	}
	ns := ""
	if r.PIDNS != nil {
		ns = *r.PIDNS
	}
	if !e.persistentRecordCheckable(ns) {
		return false
	}
	pid := int(*r.PID)
	if !proc.Exists(pid) {
		return true
	}
	if r.StartTicks == nil {
		return false
	}
	current, err := proc.Fingerprint(pid)
	return err == nil && current != *r.StartTicks
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
		return
	}
	rewriteLockFile(f, payload)
}

// Holder record of a lock file held elsewhere
func readHolderRecordIfRecorded(f *os.File) lockHolderRecord { return readHolderRecord(f) }

// Erases the holder record and lets go of the lock, a surviving record marking an abnormal exit
func releaseRebuildLock(f *os.File) {
	rewriteLockFile(f, nil)
	_ = fileutil.UnlockFile(f)
}
