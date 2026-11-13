package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Runs fn inside BEGIN IMMEDIATE and commits, retrying it with jitter while the write lock stays contended
func (s *Store) Write(ctx context.Context, patience time.Duration, fn func(c Conn) error) error {
	return presentable(s.write(ctx, patience, fn))
}

// Retry loop behind Write, returning errors as the driver and callback raised them
func (s *Store) write(ctx context.Context, patience time.Duration, fn func(c Conn) error) error {
	deadline := time.Now().Add(patience)
	var compressionDeadline time.Time
	beginIOErrRetried := false
	for {
		if err := s.refuseIfCorrupt(true); err != nil {
			return err
		}
		started, err := s.writeOnce(ctx, deadline, fn)
		if err == nil {
			s.afterWrite(ctx)
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if errors.Is(err, ErrCompressionInProgress) {
			if compressionDeadline.IsZero() {
				compressionDeadline = minTime(time.Now().Add(compressionBusyWait), deadline)
			}
			retry, serr := sleepBeforeRetry(ctx, compressionDeadline, compressionBusyWait)
			if serr != nil {
				return serr
			}
			if retry {
				continue
			}
			return err
		}
		fam := familyOf(err)
		if fam == notSQLite {
			return err
		}
		if isNoMoreRows(err) {
			retry, serr := sleepBeforeRetry(ctx, deadline, patience)
			if serr != nil {
				return serr
			}
			if retry {
				continue
			}
		}
		if fam == callFamily {
			if isLockError(err) {
				retry, serr := sleepBeforeRetry(ctx, deadline, patience)
				if serr != nil {
					return serr
				}
				if retry {
					continue
				}
				logWriteLockHolders(s.path, patience)
				return &LockedError{Patience: patience, cause: err}
			}
			if isDiskIOError(err) && !started && !beginIOErrRetried {
				retry, serr := sleepBeforeRetry(ctx, deadline, patience)
				if serr != nil {
					return serr
				}
				if retry {
					beginIOErrRetried = true
					continue
				}
			}
			return err
		}
		text := lowerText(err)
		if containsAny(text, []string{"not a database"}) || IsMalformed(err) || IsFTSScopedCorruption(err) {
			if rerr := s.lockedReplacedCheck(ctx); rerr != nil {
				return rerr
			}
		}
		if s.tables.failOpen != nil {
			retry, ferr := s.tables.failOpen(ctx, s, err, deadline, patience)
			if ferr != nil {
				return ferr
			}
			if retry {
				continue
			}
		}
		if structuralWriteCorruption(err) {
			return s.haltCorrupt(err)
		}
		return err
	}
}

// Earlier of two instants
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// One BEGIN IMMEDIATE, callback and commit under the writer lock, its busy wait never outlasting the patience left
func (s *Store) writeOnce(ctx context.Context, deadline time.Time, fn func(c Conn) error) (bool, error) {
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.release()
	if err := s.refuseIfReplaced(ctx); err != nil {
		return false, err
	}
	if s.conn == nil {
		if err := s.reopenAfterClose(ctx, "write"); err != nil {
			return false, err
		}
	}
	if left := time.Until(deadline); left < writerBusyTimeout {
		q := s.conn.q()
		if _, err := q.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", max(left, time.Millisecond).Milliseconds())); err != nil {
			return false, err
		}
		defer func() {
			_, _ = q.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA busy_timeout=%d", writerBusyTimeout.Milliseconds()))
		}()
	}
	return transact(ctx, s.conn, "BEGIN IMMEDIATE", fn)
}

// Begins, runs fn and commits, rolling back on every other exit so no transaction outlives the call, a panic included
func transact(ctx context.Context, c *conn, begin string, fn func(c Conn) error) (started bool, err error) {
	sc, err := c.open()
	if err != nil {
		return false, err
	}
	r := c.lend(true)
	committed := false
	defer func() {
		r.closeRows()
		if !committed {
			_, _ = sc.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if _, err := sc.ExecContext(ctx, begin); err != nil {
		return false, err
	}
	if err := fn(r); err != nil {
		return true, err
	}
	r.closeRows()
	if err := r.lostTransaction(); err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if _, err := sc.ExecContext(context.WithoutCancel(ctx), "COMMIT"); err != nil {
		return true, err
	}
	committed = true
	return true, nil
}

// Counts a successful write, checkpointing and merging the full-text index on their intervals
func (s *Store) afterWrite(ctx context.Context) {
	n := s.writes.Add(1)
	if n%checkpointEveryWrites == 0 {
		s.tryCheckpoint(ctx)
	}
	if n%ftsMergeEveryWrites == 0 && s.tables.merge != nil {
		s.tables.merge(ctx, s)
	}
}

// Runs one statement through Write
func (s *Store) WriteSQL(ctx context.Context, patience time.Duration, query string, args ...any) error {
	return s.Write(ctx, patience, func(c Conn) error {
		_, err := c.ExecContext(ctx, query, args...)
		return err
	})
}

// Runs one statement once per argument row through a single Write
func (s *Store) WriteMany(ctx context.Context, patience time.Duration, query string, rows [][]any) error {
	return s.Write(ctx, patience, func(c Conn) error {
		for _, args := range rows {
			if _, err := c.ExecContext(ctx, query, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// Runs one UPDATE or DELETE through Write and returns the rows it changed
func (s *Store) WriteRowcount(ctx context.Context, patience time.Duration, query string, args ...any) (int64, error) {
	var changed int64
	err := s.Write(ctx, patience, func(c Conn) error {
		res, err := c.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		if changed, err = res.RowsAffected(); err != nil || changed < 0 {
			return c.QueryRowContext(ctx, "SELECT changes()").Scan(&changed)
		}
		return nil
	})
	return changed, err
}

// Sleeps one jitter interval when the budget allows, fast early and slow after two seconds, never past the deadline
func sleepBeforeRetry(ctx context.Context, deadline time.Time, patience time.Duration) (bool, error) {
	now := time.Now()
	if !now.Before(deadline) {
		return false, nil
	}
	lo, hi := writeRetryMin, writeRetryMax
	if now.Sub(deadline.Add(-patience)) >= writeRetrySlowAt {
		lo, hi = writeRetrySlowMin, writeRetrySlowMax
	}
	if err := sleepCtx(ctx, min(jitter(lo, hi), max(deadline.Sub(now), time.Millisecond))); err != nil {
		return false, err
	}
	return true, nil
}

// Bare corruption outside the full-text index and outside an existing quarantine, never repairable from the write path
func structuralWriteCorruption(err error) bool {
	var quarantined *CorruptError
	return familyOf(err) != notSQLite && !errors.As(err, &quarantined) && !IsFTSScopedCorruption(err) &&
		Classify(err) == CauseCorrupt && corruptByCode(err)
}

// Runs the replaced and lost generation probe under the writer lock
func (s *Store) lockedReplacedCheck(ctx context.Context) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	return s.refuseIfReplaced(ctx)
}

// Quarantine refusal of this handle, or of any handle once the process latched the file corrupt
func (s *Store) refuseIfCorrupt(storage bool) error {
	s.flagMu.Lock()
	corrupt := s.corrupt
	s.flagMu.Unlock()
	if corrupt {
		return s.corruptError("")
	}
	if storage && StorageHealth(s.path) == HealthCorrupt {
		reason, _ := CorruptReason(s.path)
		return s.haltCorrupt(contentErr(nil, "database disk image is malformed (reported earlier in this process: %s)", reason))
	}
	return nil
}

// Quarantine error of this handle with its first cause
func (s *Store) corruptError(prefix string) error {
	s.flagMu.Lock()
	defer s.flagMu.Unlock()
	return &CorruptError{prefix: prefix, Reason: s.corruptReason, cause: s.corruptCause}
}

// Quarantines this handle after structural corruption, publishing the process latch first, and returns the quarantine error
func (s *Store) haltCorrupt(cause error) error {
	s.flagMu.Lock()
	s.corrupt, s.corruptReason, s.corruptCause = true, errText(cause), cause
	s.flagMu.Unlock()
	MarkCorrupt(s.path, cause)
	slog.Error(fmt.Sprintf("state.db %s reported structural corruption outside the FTS "+
		"indexes (%s); quarantining this handle: no further writes, no "+
		"automatic reopen, no explicit WAL checkpoint at close. Stop the "+
		"gateway and run `icebath sessions recover --source %s --inspect-only`.", s.path, errText(cause), s.path))
	return s.corruptError("")
}

// Sticky replaced and lost generation refusals, then the live probe, caller holding the writer lock
func (s *Store) refuseIfReplaced(ctx context.Context) error {
	s.flagMu.Lock()
	replaced, lost := s.replaced, s.walLost
	s.flagMu.Unlock()
	if replaced {
		return ErrReplaced
	}
	if lost {
		return ErrDeletedWAL
	}
	return s.haltIfGenerationChanged(ctx)
}

// Stops writes for good once the file was replaced or its WAL generation is gone, capturing a lost generation first
func (s *Store) haltIfGenerationChanged(ctx context.Context) error {
	s.flagMu.Lock()
	replaced, lost := s.replaced, s.walLost
	s.flagMu.Unlock()
	if replaced || s.fileWasReplaced() {
		s.flagMu.Lock()
		s.replaced = true
		s.flagMu.Unlock()
		s.fenceWriter()
		slog.Error(replacedMsg)
		return ErrReplaced
	}
	if lost || s.walGenerationLost() {
		s.flagMu.Lock()
		s.walLost = true
		s.flagMu.Unlock()
		s.fenceWriter()
		if _, err := s.captureGeneration(ctx, "halt"); err != nil {
			slog.Error(fmt.Sprintf("Could not capture the retired WAL generation of %s at halt: %s. Close retries "+
				"the capture and refuses to settle without it.", s.path, errText(err)))
		}
		slog.Error(deletedWALMsg)
		return ErrDeletedWAL
	}
	return nil
}

// Reports whether the path no longer names the file this store opened, by inode or by a nonzero application id
func (s *Store) fileWasReplaced() bool {
	if s.identityKnown {
		if now, ok := statIdentity(s.path); !ok || now != s.identity {
			return true
		}
	}
	if s.appID == 0 {
		return false
	}
	disk, ok := s.eng.readApplicationID(s.path)
	return ok && disk != 0 && disk != s.appID
}

// Reports whether the WAL or SHM generation this store opened is gone, adopting a generation that appeared after open
func (s *Store) walGenerationLost() bool {
	if len(s.sidecars) > 0 {
		for suffix, id := range s.sidecars {
			if now, ok := statIdentity(s.path + suffix); !ok || now != id {
				return true
			}
		}
		return false
	}
	if !s.walActive.Load() {
		return false
	}
	lost, err := ownDeletedSidecar(s.path)
	if err == nil && lost {
		return true
	}
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return false
	}
	if current := sidecarIdentities(s.path); len(current) > 0 {
		s.sidecars = current
	}
	return false
}

// Captures the lost WAL generation once per handle, later calls returning the first capture
func (s *Store) captureGeneration(ctx context.Context, trigger string) (string, error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	if s.capture != "" {
		return s.capture, nil
	}
	sidecars := make(map[string]fileIdentity, len(s.sidecars))
	for k, v := range s.sidecars {
		sidecars[k] = v
	}
	artifact, err := s.eng.captureRetiredGeneration(ctx, s.path, sidecars, trigger)
	if err != nil {
		return "", err
	}
	s.capture = artifact
	slog.Warn(fmt.Sprintf("Captured the retired WAL generation of %s at %s to %s; read its manifest.json before deciding "+
		"whether those frames belong on top of the file now at the path.", s.path, trigger, artifact))
	return artifact, nil
}

// Why this handle must not checkpoint or reopen, empty when it is healthy
func (s *Store) quarantineReason() string {
	s.flagMu.Lock()
	defer s.flagMu.Unlock()
	switch {
	case s.corrupt:
		return fmt.Sprintf("structural corruption (%s)", s.corruptReason)
	case s.replaced:
		return "a replaced state.db file"
	case s.walLost:
		return "a deleted WAL generation (split-brain)"
	}
	return ""
}

// Passive WAL checkpoint that never blocks writers, skipped on a quarantined or replaced handle and logged when it fails
func (s *Store) tryCheckpoint(ctx context.Context) {
	if s.quarantineReason() != "" {
		return
	}
	if err := s.acquire(ctx); err != nil {
		return
	}
	defer s.release()
	if s.conn == nil || s.refuseIfReplaced(ctx) != nil {
		return
	}
	if len(s.guard) > 0 {
		s.guard = s.eng.guardHold(s.path, s.guard)
	}
	var busy, logFrames, done sql.NullInt64
	err := s.conn.q().QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &logFrames, &done)
	if err != nil {
		slog.Warn(fmt.Sprintf("WAL checkpoint (PASSIVE) failed: %s", errText(err)))
		return
	}
	if logFrames.Int64 > 0 {
		slog.Debug(fmt.Sprintf("WAL checkpoint: %d/%d pages checkpointed", done.Int64, logFrames.Int64))
	}
}

// Reopens the writer after Close raced a live caller, refusing a quarantined or replaced file, caller holding the writer lock
func (s *Store) reopenAfterClose(ctx context.Context, purpose string) error {
	if s.readOnly {
		return fmt.Errorf("%w: %s (read-only handle); cannot serve a %s after close", ErrClosed, s.path, purpose)
	}
	s.flagMu.Lock()
	corrupt, replaced := s.corrupt, s.replaced
	s.flagMu.Unlock()
	if corrupt && !(replaced || s.fileWasReplaced()) {
		return s.corruptError(fmt.Sprintf("state.db connection for %s is quarantined after structural corruption; "+
			"refusing to reopen for a %s after close. ", s.path, purpose))
	}
	if err := s.haltIfGenerationChanged(ctx); err != nil {
		return err
	}
	slog.Warn(fmt.Sprintf("state.db connection for %s was closed while a %s was still in flight — reopening "+
		"(teardown/worker race)", s.path, purpose))
	c, err := s.openWriterConn(ctx)
	if err != nil {
		return callErr(err, "state.db connection was closed while a %s was still in flight (a session-teardown path "+
			"called close before this worker finished) and the automatic reopen failed: %s", purpose, errText(err))
	}
	s.conn = c
	s.writerUp.Store(true)
	if s.walActive.Load() {
		s.guard = s.eng.guardHold(s.path, nil)
	}
	return nil
}
