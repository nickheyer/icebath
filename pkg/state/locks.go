package state

import (
	"database/sql"
	"errors"
	"time"
)

// Claims a lease row unless a live holder other than the claimant has it
func claimLease(tx *Tx, table, keyCol, key, holder string, ttl time.Duration) (bool, error) {
	var current string
	var expires float64
	err := tx.QueryRow("SELECT holder, expires_at FROM "+table+" WHERE "+keyCol+" = ?", key).Scan(&current, &expires)
	t := now()
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	case current != holder && expires > t:
		return false, nil
	}
	_, err = tx.Exec("INSERT INTO "+table+" ("+keyCol+", holder, acquired_at, expires_at) VALUES (?, ?, ?, ?) ON CONFLICT("+keyCol+") DO UPDATE SET holder = excluded.holder, acquired_at = excluded.acquired_at, expires_at = excluded.expires_at",
		key, holder, t, t+ttl.Seconds())
	return err == nil, err
}

// Takes the compression lock of a session for holder, false when another live holder has it
func (d *DB) TryAcquireCompressionLock(sessionID, holder string, ttl time.Duration) (bool, error) {
	ok := false
	err := d.write(func(tx *Tx) error {
		var err error
		ok, err = claimLease(tx, "compression_locks", "session_id", sessionID, holder, ttl)
		return err
	})
	return ok, err
}

// Extends the compression lock of a holder, false when it was lost
func (d *DB) RefreshCompressionLock(sessionID, holder string, ttl time.Duration) (bool, error) {
	ok := false
	err := d.write(func(tx *Tx) error {
		res, err := tx.Exec("UPDATE compression_locks SET expires_at = ? WHERE session_id = ? AND holder = ?", now()+ttl.Seconds(), sessionID, holder)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		ok = n > 0
		return nil
	})
	return ok, err
}

// Releases the compression lock a holder took
func (d *DB) ReleaseCompressionLock(sessionID, holder string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("DELETE FROM compression_locks WHERE session_id = ? AND holder = ?", sessionID, holder)
		return err
	})
}

// Holder of a live compression lock, empty when none
func (d *DB) CompressionLockHolder(sessionID string) (string, error) {
	var holder string
	var expires float64
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT holder, expires_at FROM compression_locks WHERE session_id = ?", sessionID).Scan(&holder, &expires)
	})
	if errors.Is(err, sql.ErrNoRows) || (err == nil && expires <= now()) {
		return "", nil
	}
	return holder, err
}

// Takes the turn lease of a conversation for holder, false when another live holder has it
func (d *DB) TryAcquireTurnLease(sessionID, holder string, ttl time.Duration) (bool, error) {
	ok := false
	err := d.write(func(tx *Tx) error {
		var err error
		ok, err = claimLease(tx, "session_turn_leases", "conversation_id", sessionID, holder, ttl)
		return err
	})
	return ok, err
}

// Extends the turn lease of a holder, false when it was lost
func (d *DB) RefreshTurnLease(sessionID, holder string, ttl time.Duration) (bool, error) {
	ok := false
	err := d.write(func(tx *Tx) error {
		res, err := tx.Exec("UPDATE session_turn_leases SET expires_at = ? WHERE conversation_id = ? AND holder = ?", now()+ttl.Seconds(), sessionID, holder)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		ok = n > 0
		return nil
	})
	return ok, err
}

// Releases the turn lease a holder took
func (d *DB) ReleaseTurnLease(sessionID, holder string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("DELETE FROM session_turn_leases WHERE conversation_id = ? AND holder = ?", sessionID, holder)
		return err
	})
}

// Compression failure cooldown of a session
type CompressionCooldown struct {
	Until float64
	Error string
}

// Records a compression failure cooldown on a session
func (d *DB) SetCompressionCooldown(sessionID string, until float64, reason string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET compression_failure_cooldown_until = ?, compression_failure_error = ? WHERE id = ?", until, nullStr(reason), sessionID)
		return err
	})
}

// Clears the compression failure cooldown of a session
func (d *DB) ClearCompressionCooldown(sessionID string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET compression_failure_cooldown_until = NULL, compression_failure_error = NULL WHERE id = ?", sessionID)
		return err
	})
}

// Compression failure cooldown of a session, zero when none
func (d *DB) CompressionCooldown(sessionID string) (CompressionCooldown, error) {
	var until sql.NullFloat64
	var reason sql.NullString
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT compression_failure_cooldown_until, compression_failure_error FROM sessions WHERE id = ?", sessionID).Scan(&until, &reason)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CompressionCooldown{}, nil
	}
	return CompressionCooldown{Until: until.Float64, Error: reason.String}, err
}

// Consecutive deterministic fallback summaries of a session
func (d *DB) CompressionFallbackStreak(sessionID string) (int, error) {
	var n int
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT compression_fallback_streak FROM sessions WHERE id = ?", sessionID).Scan(&n)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Sets the fallback summary streak of a session
func (d *DB) SetCompressionFallbackStreak(sessionID string, streak int) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET compression_fallback_streak = ? WHERE id = ?", streak, sessionID)
		return err
	})
}

// Consecutive compactions that reclaimed nothing
func (d *DB) CompressionIneffectiveCount(sessionID string) (int, error) {
	var n int
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT compression_ineffective_count FROM sessions WHERE id = ?", sessionID).Scan(&n)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Sets the ineffective compaction count of a session
func (d *DB) SetCompressionIneffectiveCount(sessionID string, count int) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET compression_ineffective_count = ? WHERE id = ?", count, sessionID)
		return err
	})
}
