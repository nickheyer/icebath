package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Bounded wire codes a runtime refusal carries
const (
	ReasonInvalidParams      = "invalid_params"
	ReasonStaleEpoch         = "stale_epoch"
	ReasonNotFound           = "not_found"
	ReasonAdmissionConflict  = "admission_conflict"
	ReasonQueueFull          = "queue_full"
	ReasonUnknownExecution   = "unknown_execution"
	ReasonSessionBusy        = "session_busy"
	ReasonStaleGeneration    = "stale_generation"
	ReasonAdmissionStarted   = "admission_started"
	ReasonSessionIdle        = "session_idle"
	ReasonRevisionConflict   = "revision_conflict"
	ReasonPermissionDenied   = "permission_denied"
	ReasonStorageUnavailable = "storage_unavailable"
	ReasonSessionExists      = "session_exists"
	ReasonSessionDeleted     = "session_deleted"
)

// Refusal by the state authority, Reason the wire code and Field and Detail naming the check that fired
type RuntimeStoreError struct {
	Reason string
	Field  string
	Detail string
}

// Wire code of the refusal
func (e *RuntimeStoreError) Error() string { return e.Reason }

// Refusal carrying only its wire code
func runtimeErr(reason string) error { return &RuntimeStoreError{Reason: reason} }

// Refusal naming the field and detail of the check that fired, each cut to one bounded printable line
func runtimeErrAt(reason, field, detail string) error {
	return &RuntimeStoreError{Reason: reason, Field: oneLine(field, 80), Detail: oneLine(detail, 160)}
}

// Reports whether err is a runtime refusal with the given wire code
func IsRuntimeReason(err error, reason string) bool {
	var re *RuntimeStoreError
	return errors.As(err, &re) && re.Reason == reason
}

// Conflict proving the request id was already admitted with another payload
func admittedRequestConflict() error {
	return runtimeErrAt(ReasonAdmissionConflict, "request_id", "this request id was already admitted with another payload")
}

// Text as one printable line of at most limit characters, whitespace collapsed
func oneLine(value string, limit int) string {
	collapsed := strings.Join(previewFields(value), " ")
	var b strings.Builder
	n := 0
	for _, r := range collapsed {
		if n >= limit {
			break
		}
		if r == ' ' || unicode.IsPrint(r) {
			b.WriteRune(r)
			n++
		}
	}
	return b.String()
}

// Refuses an identifier that is empty or longer than a kilobyte of characters
func checkText(values ...string) error {
	for _, v := range values {
		if v == "" || utf8.RuneCountInString(v) > 1024 {
			return runtimeErr(ReasonInvalidParams)
		}
	}
	return nil
}

// Canonical JSON text of an object, keys sorted and no padding, refusing anything that is not one object
func canonicalObject(raw json.RawMessage) (string, error) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t[0] != '{' || !json.Valid([]byte(t)) {
		return "", runtimeErr(ReasonInvalidParams)
	}
	enc, err := fileutil.EncodeJSON(json.RawMessage(t), fileutil.JSONStyle{Tight: true, Sorted: true})
	if err != nil {
		return "", runtimeErr(ReasonInvalidParams)
	}
	return string(enc), nil
}

// Canonical JSON text of a value the store built itself
func canonicalJSON(v any) (string, error) {
	enc, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{Tight: true, Sorted: true})
	return string(enc), err
}

// Refuses a write or read presented under an owner epoch that is no longer current
func checkEpoch(ctx context.Context, c Conn, epoch int64) error {
	var current sql.NullInt64
	err := c.QueryRowContext(ctx, "SELECT epoch FROM runtime_epoch WHERE singleton=1").Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!current.Valid || current.Int64 != epoch)) {
		return runtimeErr(ReasonStaleEpoch)
	}
	return err
}

// Runtime revision and generation of a session row, a not found refusal when it is gone
func runtimeCounters(ctx context.Context, c Conn, sessionID string) (revision, generation int64, err error) {
	err = c.QueryRowContext(ctx, "SELECT runtime_revision, runtime_generation FROM sessions WHERE id=?", sessionID).Scan(&revision, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, runtimeErr(ReasonNotFound)
	}
	return revision, generation, err
}

// Reports whether a state_meta key exists on c
func metaExists(ctx context.Context, c Conn, key string) (bool, error) {
	_, ok, err := metaValue(ctx, c, key)
	return ok, err
}

// Starts the next owner epoch for the instance and returns it, the caller holding the profile ownership lock
func (s *Store) BeginRuntimeEpoch(ctx context.Context, instanceID string) (int64, error) {
	if err := checkText(instanceID); err != nil {
		return 0, err
	}
	var epoch int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		if _, err := c.ExecContext(ctx, `INSERT INTO runtime_epoch(singleton,epoch,instance_id) VALUES(1,1,?)
            ON CONFLICT(singleton) DO UPDATE SET epoch=runtime_epoch.epoch+1, instance_id=excluded.instance_id`, instanceID); err != nil {
			return err
		}
		return c.QueryRowContext(ctx, "SELECT epoch FROM runtime_epoch WHERE singleton=1").Scan(&epoch)
	})
	return epoch, err
}
