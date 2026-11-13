package state

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// Runs the idempotent read fn, replaying it on the same connection through a transient disk I/O error and latching corruption
func (s *Store) Read(ctx context.Context, fn func(c Conn) error) error {
	return presentable(s.read(ctx, fn))
}

// Retry loop behind Read, returning errors as the driver and callback raised them
func (s *Store) read(ctx context.Context, fn func(c Conn) error) error {
	for attempt := 0; ; attempt++ {
		err := s.readConn(ctx, true, fn)
		if err == nil {
			return nil
		}
		switch familyOf(err) {
		case callFamily:
			if ctx.Err() != nil || attempt >= readIOErrRetries || !isDiskIOError(err) {
				NoteStorageError(s.path, err)
				return err
			}
			if serr := sleepCtx(ctx, readIOErrBackoff); serr != nil {
				return serr
			}
		case contentFamily:
			NoteStorageError(s.path, err)
			return err
		default:
			return err
		}
	}
}

// Runs fn once on a read connection without reopening a closed store, ErrClosed after Close
func (s *Store) ReadIfOpen(ctx context.Context, fn func(c Conn) error) error {
	return presentable(s.readConn(ctx, false, fn))
}

// Lends fn a pooled read-only connection without the writer lock under WAL, else the writer connection under its lock
func (s *Store) readConn(ctx context.Context, reopen bool, fn func(c Conn) error) error {
	c, err := s.checkoutRead(ctx)
	if err != nil {
		return err
	}
	if c != nil {
		r := c.lend(false)
		defer s.returnRead(ctx, c, r)
		return fn(r)
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.conn == nil {
		if !reopen {
			return fmt.Errorf("%w: %s", ErrClosed, s.path)
		}
		if err := s.reopenAfterClose(ctx, "read"); err != nil {
			return err
		}
	}
	r := s.conn.lend(false)
	defer s.settleWriterRead(ctx, r)
	if s.walActive.Load() || s.readOnly {
		return fn(r)
	}
	q := s.conn.q()
	var previous sql.NullInt64
	if err := q.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&previous); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", readBusyTimeout.Milliseconds())); err != nil {
		return err
	}
	defer func() {
		_, _ = q.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA busy_timeout=%d", previous.Int64))
	}()
	return fn(r)
}

// Settles a read the writer served, letting the writer go when it stays inside a transaction
func (s *Store) settleWriterRead(ctx context.Context, r *runner) {
	if err := r.settle(ctx); err != nil {
		slog.Error(fmt.Sprintf("state.db writer connection for %s could not be returned to autocommit after a read (%s); "+
			"letting it go so the next statement reopens it", s.path, errText(err)))
		s.dropWriter()
	}
}

// Pooled read connection, a fresh one on a miss, nil when reads must take the writer instead
func (s *Store) checkoutRead(ctx context.Context) (*conn, error) {
	if !s.walActive.Load() || s.readOnly {
		return nil, nil
	}
	s.readMu.Lock()
	if n := len(s.pool); n > 0 {
		c := s.pool[n-1]
		s.pool = s.pool[:n-1]
		s.readMu.Unlock()
		return c, nil
	}
	s.readMu.Unlock()
	return s.openReadConn(ctx)
}

// Opens a read connection under a permit taken first, backing off a minute after a failed open
func (s *Store) openReadConn(ctx context.Context) (*conn, error) {
	s.readMu.Lock()
	backingOff := !s.readFailedAt.IsZero() && time.Since(s.readFailedAt) < readOpenRetryAfter
	closed := s.readClosed
	s.readMu.Unlock()
	if closed || backingOff {
		return nil, nil
	}
	if !s.eng.acquireRead(s.budget, s) {
		slog.Debug(fmt.Sprintf("read pool at capacity (%d) for %s; serving this read from the locked writer connection", readPoolMax, s.path))
		return nil, nil
	}
	c, err := s.connectReadOnly(ctx)
	if err == nil {
		applyDatabasePragmas(ctx, c.q(), s.settings, storeLabel)
		if s.tables.connSetup != nil {
			if err = lendTo(ctx, c, func(q Conn) error { return s.tables.connSetup(ctx, s, q, false) }); err != nil {
				closeLogged(c, "partially-opened read conn", s.path)
			}
		}
	}
	if err != nil {
		s.eng.releaseRead(s.budget)
		if familyOf(err) == notSQLite {
			return nil, err
		}
		s.readMu.Lock()
		s.readFailedAt = time.Now()
		s.readMu.Unlock()
		slog.Debug(fmt.Sprintf("read-only connection open failed for %s: %s", s.path, errText(err)))
		return nil, nil
	}
	return c, nil
}

// Puts a settled read connection back in the pool, closing it when unsettled, the store closed or the pool full
func (s *Store) returnRead(ctx context.Context, c *conn, r *runner) {
	if err := r.settle(ctx); err != nil {
		slog.Warn(fmt.Sprintf("read-conn for %s could not be returned to autocommit (%s); closing it", s.path, errText(err)))
		s.closeRead(c)
		return
	}
	s.readMu.Lock()
	if !s.readClosed && len(s.pool) < readPoolMax {
		s.pool = append(s.pool, c)
		s.readMu.Unlock()
		return
	}
	s.readMu.Unlock()
	s.closeRead(c)
}

// Closes one idle pooled connection for a peer that wants its permit, never one in use
func (s *Store) evictIdleRead() bool {
	s.readMu.Lock()
	n := len(s.pool)
	if n == 0 {
		s.readMu.Unlock()
		return false
	}
	c := s.pool[n-1]
	s.pool = s.pool[:n-1]
	s.readMu.Unlock()
	s.closeRead(c)
	return true
}

// Closes a read connection and returns its permits even when the close fails
func (s *Store) closeRead(c *conn) {
	closeLogged(c, "read-conn", s.path)
	s.eng.releaseRead(s.budget)
}
