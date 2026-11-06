package state

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const cooldownRowSQL = "SELECT compression_failure_cooldown_until, compression_failure_error FROM sessions WHERE id = ?"

// Active compression failure cooldown of a session, the zero value when none is live
type CompressionCooldown struct {
	Until            float64
	RemainingSeconds float64
	Error            string
}

// Exact stored cooldown columns of a session row, no expiry filtering
type CompressionCooldownRow struct {
	SessionExists bool
	CooldownUntil *float64
	Error         any
}

// Dict form of the row in hermes key order for repr and comparison
func (r CompressionCooldownRow) object() *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("session_exists", r.SessionExists)
	if r.CooldownUntil == nil {
		out.Set("cooldown_until", nil)
	} else {
		out.Set("cooldown_until", *r.CooldownUntil)
	}
	out.Set("error", r.Error)
	return out
}

// Cooldown row from raw columns with the deadline cast through Python float
func cooldownRow(exists bool, cooldownUntil, errValue any) (CompressionCooldownRow, error) {
	row := CompressionCooldownRow{SessionExists: exists, Error: errValue}
	if cooldownUntil != nil {
		f, err := pyint.Float(cooldownUntil)
		if err != nil {
			return row, err
		}
		row.CooldownUntil = &f
	}
	return row, nil
}

// Persists the cooldown keeping any longer live deadline, the error always taking the latest diagnostic
func (d *SessionDB) RecordCompressionFailureCooldown(sessionID string, cooldownUntil float64, errValue *string) {
	if sessionID == "" {
		return
	}
	var stored any
	if errValue != nil {
		stored = *errValue
	}
	d.writeSQLLogged(context.Background(), "record_compression_failure_cooldown", sessionID,
		"UPDATE sessions SET compression_failure_cooldown_until = CASE "+
			"WHEN compression_failure_cooldown_until IS NOT NULL  AND compression_failure_cooldown_until > ? "+
			"THEN compression_failure_cooldown_until ELSE ? END, compression_failure_error = ? WHERE id = ?",
		[]any{cooldownUntil, cooldownUntil, stored, sessionID})
}

// Records the cooldown with an empty reason stored as NULL
func (d *DB) SetCompressionCooldown(sessionID string, until float64, reason string) error {
	var errValue *string
	if reason != "" {
		errValue = &reason
	}
	d.RecordCompressionFailureCooldown(sessionID, until, errValue)
	return nil
}

// Unexpired compression failure cooldown with its remaining seconds, nil when none is live
func (d *SessionDB) GetCompressionFailureCooldown(sessionID string) (*pyjson.Object, error) {
	current := now()
	if sessionID == "" {
		return nil, nil
	}
	row, err := d.readOne(context.Background(), cooldownRowSQL, sessionID)
	if err != nil || row == nil {
		return nil, err
	}
	raw := rowValue(row, "compression_failure_cooldown_until")
	if raw == nil {
		return nil, nil
	}
	until, err := pyint.Float(raw)
	if err != nil || until <= current {
		return nil, err
	}
	out := pyjson.NewObject()
	out.Set("cooldown_until", until)
	out.Set("remaining_seconds", until-current)
	out.Set("error", rowValue(row, "compression_failure_error"))
	return out, nil
}

// Live cooldown deadline and reason, zero when none is active
func (d *DB) CompressionCooldown(sessionID string) (CompressionCooldown, error) {
	row, err := d.GetCompressionFailureCooldown(sessionID)
	if err != nil || row == nil {
		return CompressionCooldown{}, err
	}
	out := CompressionCooldown{Until: rowValue(row, "cooldown_until").(float64), RemainingSeconds: rowValue(row, "remaining_seconds").(float64)}
	if v := rowValue(row, "error"); v != nil {
		out.Error = pyrepr.Str(v)
	}
	return out, nil
}

// Exact stored cooldown columns so a cancelled compression can roll an expired or absent row back
func (d *SessionDB) GetCompressionFailureCooldownRow(sessionID string) (CompressionCooldownRow, error) {
	if sessionID == "" {
		return cooldownRow(false, nil, nil)
	}
	row, err := d.readOne(context.Background(), cooldownRowSQL, sessionID)
	if err != nil {
		return CompressionCooldownRow{}, err
	}
	if row == nil {
		return cooldownRow(false, nil, nil)
	}
	return cooldownRow(true, rowValue(row, "compression_failure_cooldown_until"), rowValue(row, "compression_failure_error"))
}

// Restores and verifies a cooldown snapshot, tolerating only a session row that vanished mid attempt
func (d *SessionDB) RestoreCompressionFailureCooldownRow(sessionID string, snapshot CompressionCooldownRow) error {
	if !snapshot.SessionExists {
		current, err := d.GetCompressionFailureCooldownRow(sessionID)
		if err != nil {
			return err
		}
		if current.SessionExists {
			return &pyint.Error{Kind: "RuntimeError", Msg: "cannot restore absent compression cooldown row: session now exists"}
		}
		return nil
	}
	var deadline any
	if snapshot.CooldownUntil != nil {
		deadline = *snapshot.CooldownUntil
	}
	updated := false
	err := d.executeWrite(context.Background(), func(conn SQLiteConn) error {
		res, err := conn.ExecContext(context.Background(), "UPDATE sessions SET compression_failure_cooldown_until = ?, "+
			"compression_failure_error = ? WHERE id = ?", deadline, snapshot.Error, sessionID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		updated = n == 1
		return err
	}, nil)
	if err != nil {
		return err
	}
	if !updated {
		slog.Warn("compression cooldown rollback session missing: " + sessionID)
		return nil
	}
	actual, err := d.GetCompressionFailureCooldownRow(sessionID)
	if err != nil {
		return err
	}
	expected, err := cooldownRow(true, deadline, snapshot.Error)
	if err != nil {
		return err
	}
	if pyrepr.Repr(actual.object()) != pyrepr.Repr(expected.object()) {
		if !actual.SessionExists {
			slog.Warn("compression cooldown rollback session missing after restore: " + sessionID)
			return nil
		}
		return &pyint.Error{Kind: "RuntimeError", Msg: fmt.Sprintf("compression cooldown rollback verification failed: expected=%s, actual=%s",
			pyrepr.Repr(expected.object()), pyrepr.Repr(actual.object()))}
	}
	return nil
}

// Clears any persisted compression failure cooldown, logging instead of failing
func (d *SessionDB) ClearCompressionFailureCooldown(sessionID string) {
	if sessionID == "" {
		return
	}
	d.writeSQLLogged(context.Background(), "clear_compression_failure_cooldown", sessionID,
		"UPDATE sessions SET compression_failure_cooldown_until = NULL, compression_failure_error = NULL WHERE id = ?", []any{sessionID})
}

// Clears the cooldown, never failing as hermes logs the write error
func (d *DB) ClearCompressionCooldown(sessionID string) error {
	d.ClearCompressionFailureCooldown(sessionID)
	return nil
}

// Numeric sessions column clamped at zero, a missing row, NULL or unparsable value reading as zero
func (d *SessionDB) readSessionNumber(column, sessionID string, asFloat bool) (float64, error) {
	if sessionID == "" {
		return 0, nil
	}
	row, err := d.readOne(context.Background(), "SELECT "+column+" FROM sessions WHERE id = ?", sessionID)
	if err != nil || row == nil {
		return 0, err
	}
	raw := rowValue(row, column)
	if b, ok := raw.([]byte); ok {
		raw = string(b)
	}
	if !pyrepr.Truthy(raw) {
		return 0, nil
	}
	var value float64
	if asFloat {
		f, ferr := pyint.Float(raw)
		if ferr != nil {
			return 0, castFailure(ferr)
		}
		value = f
	} else {
		n, ierr := pyint.Of(raw)
		if ierr != nil {
			return 0, castFailure(ierr)
		}
		value, _ = new(big.Float).SetInt(n).Float64()
	}
	if math.IsNaN(value) || !(value > 0) {
		return 0, nil
	}
	return value, nil
}

// TypeError and ValueError read as zero, any other cast failure propagates
func castFailure(err error) error {
	if pe, ok := err.(*pyint.Error); ok && (pe.Kind == "TypeError" || pe.Kind == "ValueError") {
		return nil
	}
	return err
}

// Sets one sessions column of a row
func (d *SessionDB) writeSessionColumn(column, sessionID string, value any) error {
	return d.writeSQL(context.Background(), "UPDATE sessions SET "+column+" = ? WHERE id = ?", []any{value, sessionID}, false, nil)
}

// Persisted deterministic fallback streak
func (d *DB) CompressionFallbackStreak(sessionID string) (int, error) {
	v, err := d.readSessionNumber("compression_fallback_streak", sessionID, false)
	return int(v), err
}

// Persists the deterministic fallback streak clamped at zero
func (d *DB) SetCompressionFallbackStreak(sessionID string, streak int) error {
	if sessionID == "" {
		return nil
	}
	return d.writeSessionColumn("compression_fallback_streak", sessionID, max(0, streak))
}

// Persisted ineffective compaction strike count a resumed compressor inherits
func (d *DB) CompressionIneffectiveCount(sessionID string) (int, error) {
	v, err := d.readSessionNumber("compression_ineffective_count", sessionID, false)
	return int(v), err
}

// Persists the ineffective compaction strike count clamped at zero
func (d *DB) SetCompressionIneffectiveCount(sessionID string, count int) error {
	if sessionID == "" {
		return nil
	}
	return d.writeSessionColumn("compression_ineffective_count", sessionID, max(0, count))
}

// Persisted anti thrash recovery deadline in epoch seconds, zero when not armed
func (d *DB) CompressionRecoveryDeadline(sessionID string) (float64, error) {
	return d.readSessionNumber("compression_recovery_deadline", sessionID, true)
}

// Persists the recovery deadline, zero or a non positive value disarming it as NULL
func (d *DB) SetCompressionRecoveryDeadline(sessionID string, deadline float64) error {
	if sessionID == "" {
		return nil
	}
	if !(deadline > 0) {
		return d.writeSessionColumn("compression_recovery_deadline", sessionID, nil)
	}
	return d.writeSessionColumn("compression_recovery_deadline", sessionID, deadline)
}

// Persisted sustained overload abort streak
func (d *DB) CompressionOverloadStreak(sessionID string) (int, error) {
	v, err := d.readSessionNumber("compression_overload_streak", sessionID, false)
	return int(v), err
}

// Persists the sustained overload abort streak clamped at zero
func (d *DB) SetCompressionOverloadStreak(sessionID string, streak int) error {
	if sessionID == "" {
		return nil
	}
	return d.writeSessionColumn("compression_overload_streak", sessionID, max(0, streak))
}

// Atomically bumps the overload streak returning the new value, nil when the row is absent
func (d *DB) IncrementCompressionOverloadStreak(sessionID string) (*int, error) {
	if sessionID == "" {
		return nil, nil
	}
	var out *int
	err := d.executeWrite(context.Background(), func(conn SQLiteConn) error {
		row, err := connRow(context.Background(), conn, "UPDATE sessions SET compression_overload_streak = compression_overload_streak + 1"+
			" WHERE id = ? RETURNING compression_overload_streak", sessionID)
		if err != nil || row == nil {
			out = nil
			return err
		}
		n, err := pyint.Of(row["compression_overload_streak"])
		if err != nil {
			return err
		}
		v := int(n.Int64())
		out = &v
		return nil
	}, nil)
	return out, err
}
