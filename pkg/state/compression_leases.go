package state

import (
	"context"
	"fmt"
	"log/slog"
	"math"
)

const turnLeaseSweepGraceS = 86400.0

// Single transaction lease claim reclaiming a stale holder, returning ownership and the reclaimed holder
func claimLeaseRow(ctx context.Context, conn SQLiteConn, table, keyCol, key, holder string, now, expiresAt float64, stale func(holder, expiresAt any) bool) (bool, any, error) {
	var reclaimed any
	row, err := connRow(ctx, conn, "SELECT holder, expires_at FROM "+table+" WHERE "+keyCol+" = ?", key)
	if err != nil {
		return false, nil, err
	}
	if row != nil && stale(row["holder"], row["expires_at"]) {
		if _, err := conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+keyCol+" = ? AND holder = ?", key, row["holder"]); err != nil {
			return false, nil, err
		}
		reclaimed = row["holder"]
	}
	if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+" ("+keyCol+", holder, acquired_at, expires_at) VALUES (?, ?, ?, ?)",
		key, holder, now, expiresAt); err != nil {
		return false, nil, err
	}
	owner, err := connRow(ctx, conn, "SELECT holder FROM "+table+" WHERE "+keyCol+" = ?", key)
	if err != nil {
		return false, nil, err
	}
	return owner != nil && owner["holder"] == holder, reclaimed, nil
}

// Write that logs a warning instead of failing on a database error
func (d *SessionDB) writeSQLLogged(ctx context.Context, op, sessionID, query string, params []any) {
	if err := d.writeSQL(ctx, query, params, false, nil); err != nil {
		slog.Warn(fmt.Sprintf("%s(%s) failed: %s", op, sessionID, errText(err)))
	}
}

// Extends the compression lock while holder still owns its row, even past the expiry
func (d *SessionDB) RefreshCompressionLock(sessionID, holder string, ttlSeconds float64) bool {
	if sessionID == "" || holder == "" {
		return false
	}
	n, err := d.writeRowcount(context.Background(), "UPDATE compression_locks SET expires_at = ? WHERE session_id = ? AND holder = ?",
		[]any{now() + ttlSeconds, sessionID, holder}, nil)
	if err != nil {
		slog.Warn(fmt.Sprintf("refresh_compression_lock(%s) failed: %s", sessionID, errText(err)))
		return false
	}
	return n > 0
}

// Takes the compression lock, reclaiming expired or dead pid holders, false when a live holder owns it
func (d *SessionDB) TryAcquireCompressionLock(sessionID, holder string, ttlSeconds float64) bool {
	if sessionID == "" {
		return false
	}
	ctx := context.Background()
	t := now()
	var acquired bool
	var reclaimed any
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		acquired, reclaimed, err = claimLeaseRow(ctx, conn, "compression_locks", "session_id", sessionID, holder, t, t+ttlSeconds, func(h, e any) bool {
			expires, _ := pyFloatOf(e)
			name, _ := h.(string)
			return expires < t || compressionLockHolderProcessIsDead(name)
		})
		return err
	}, nil)
	if err != nil {
		slog.Warn(fmt.Sprintf("try_acquire_compression_lock(%s) failed: %s", sessionID, errText(err)))
		return false
	}
	if pyTruthy(reclaimed) {
		slog.Warn(fmt.Sprintf("Reclaimed stale compression lock for session=%s (holder=%s)", sessionID, pyStr(reclaimed)))
	}
	return acquired
}

// Releases the compression lock when holder owns it, idempotent once gone
func (d *SessionDB) ReleaseCompressionLock(sessionID, holder string) {
	if sessionID == "" {
		return
	}
	d.writeSQLLogged(context.Background(), "release_compression_lock", sessionID,
		"DELETE FROM compression_locks WHERE session_id = ? AND holder = ?", []any{sessionID, holder})
}

// Current unexpired compression lock holder, nil when none
func (d *SessionDB) GetCompressionLockHolder(sessionID string) (*string, error) {
	if sessionID == "" {
		return nil, nil
	}
	row, err := d.readOne(context.Background(), "SELECT holder FROM compression_locks WHERE session_id = ? AND expires_at >= ?", sessionID, now())
	if err != nil || row == nil {
		return nil, err
	}
	holder := pyStr(rowValue(row, "holder"))
	return &holder, nil
}

// Test overrides of the turn lease steps hermes tests monkeypatch, nil fields running the real step
type turnLeaseSeams struct {
	keyOnConn  func(ctx context.Context, conn SQLiteConn, sessionID string) (string, error)
	key        func(sessionID string) (string, error)
	tryAcquire func(sessionID, holder string, ttlSeconds float64, patienceS *float64) (bool, error)
	sleepRetry func(deadline, patienceS float64) bool
}

// Installs the sleep every write of this store runs between lock retries, nil restoring the jittered sleep
func (d *SessionDB) SetWriteRetrySleep(fn func(deadline, patienceS float64) bool) {
	d.leaseSeams.sleepRetry = fn
}

// Conversation key the turn lease of a session is stored under
func (d *SessionDB) sessionTurnLeaseKey(ctx context.Context, sessionID string) (string, error) {
	if d.leaseSeams.key != nil {
		return d.leaseSeams.key(sessionID)
	}
	if sessionID == "" {
		return sessionID, nil
	}
	var key string
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		key, err = d.sessionTurnLeaseKeyOnConn(ctx, conn, sessionID)
		return err
	})
	return key, err
}

// Claims the conversation turn lease once after sweeping long expired rows
func (d *SessionDB) TryAcquireSessionTurnLease(sessionID, holder string, ttlSeconds float64, patienceS *float64) (bool, error) {
	if sessionID == "" || holder == "" {
		return false, nil
	}
	ctx := context.Background()
	t := now()
	expiresAt := t + math.Max(0.1, ttlSeconds)
	var acquired bool
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE expires_at < ?", t-turnLeaseSweepGraceS); err != nil {
			return err
		}
		key, err := d.sessionTurnLeaseKeyOnConn(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		acquired, _, err = claimLeaseRow(ctx, conn, "session_turn_leases", "conversation_id", key, holder, t, expiresAt, func(h, e any) bool {
			expires, _ := pyFloatOf(e)
			name, _ := h.(string)
			return expires <= t || compressionLockHolderProcessIsDead(name)
		})
		return err
	}, patienceS)
	return acquired, err
}

// Waiting policy of a blocking turn lease acquire, callbacks returning errors that are logged and ignored
type TurnLeaseWait struct {
	TTLSeconds                float64
	WaitSeconds               float64
	PollIntervalSeconds       float64
	WaitNoticeIntervalSeconds float64
	AcquirePatienceS          float64
	OnWait                    func(waited float64) error
	ShouldAbort               func() (bool, error)
	OnContended               func() error
}

// Waiting policy with the acquire_session_turn_lease defaults
func NewTurnLeaseWait() TurnLeaseWait {
	return TurnLeaseWait{TTLSeconds: 300, WaitSeconds: 1800, PollIntervalSeconds: 1, WaitNoticeIntervalSeconds: 15, AcquirePatienceS: 0.5}
}

// Polls for the conversation turn lease until acquired, aborted or out of time, notifying waiters on the way
func (d *SessionDB) AcquireSessionTurnLease(sessionID, holder string, o TurnLeaseWait) (bool, error) {
	try := d.TryAcquireSessionTurnLease
	if d.leaseSeams.tryAcquire != nil {
		try = d.leaseSeams.tryAcquire
	}
	return AcquireTurnLeasePolling("SessionDB", try, d.sleepBeforeWriteRetry, sessionID, holder, o)
}

// Expiry of the turn lease holder owns on the conversation, nil when it holds none
func (d *SessionDB) SessionTurnLeaseExpiresAt(sessionID, holder string) (*float64, error) {
	if sessionID == "" || holder == "" {
		return nil, nil
	}
	ctx := context.Background()
	var out *float64
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		key, err := d.sessionTurnLeaseKeyOnConn(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		row, err := connRow(ctx, conn, "SELECT expires_at FROM session_turn_leases WHERE conversation_id = ? AND holder = ?", key, holder)
		if err != nil || row == nil {
			return err
		}
		f, err := pyFloatOf(row["expires_at"])
		out = &f
		return err
	})
	return out, err
}

// Extends the turn lease holder owns, false when it no longer owns it
func (d *SessionDB) RefreshSessionTurnLease(sessionID, holder string, ttlSeconds float64, patienceS *float64) (bool, error) {
	if sessionID == "" || holder == "" {
		return false, nil
	}
	ctx := context.Background()
	expiresAt := now() + math.Max(0.1, ttlSeconds)
	var refreshed bool
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		key, err := d.sessionTurnLeaseKeyOnConn(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "UPDATE session_turn_leases SET expires_at = ? WHERE conversation_id = ? AND holder = ?", expiresAt, key, holder)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		refreshed = n > 0
		return err
	}, patienceS)
	return refreshed, err
}

// Releases the turn lease holder owns on the conversation
func (d *SessionDB) ReleaseSessionTurnLease(sessionID, holder string) error {
	if sessionID == "" || holder == "" {
		return nil
	}
	ctx := context.Background()
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		key, err := d.sessionTurnLeaseKeyOnConn(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE conversation_id = ? AND holder = ?", key, holder)
		return err
	}, nil)
}
