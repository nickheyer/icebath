package gateway

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Missing mapping key of a hosted room record, a KeyError
type HostedKeyError struct{ Key string }

func (e *HostedKeyError) Error() string { return pyrepr.Repr(e.Key) }

// Python class name of the error
func (e *HostedKeyError) PyTypeName() string { return "KeyError" }

// Non blank string bounded by maxBytes of UTF-8, strip false keeping and measuring the raw text
func roomText(value any, label string, maxBytes int, strip bool, fail roomErrorFunc) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fail(label + " must be a string")
	}
	if pystr.Strip(text) == "" {
		return "", fail(label + " must not be empty")
	}
	if strip {
		text = pystr.Strip(text)
	}
	if len(text) > maxBytes {
		return "", fail(label + " is too large")
	}
	return text, nil
}

// Compact sorted JSON bounded by maxBytes of UTF-8, unserializable input failing with the module error
func roomCanonicalJSON(value any, label string, maxBytes int, ensureASCII bool, fail roomErrorFunc) (string, error) {
	encoded, err := roomCompactJSON(value, ensureASCII)
	if err != nil {
		return "", fail(label + " must be JSON-serializable")
	}
	if len(encoded) > maxBytes {
		return "", fail(label + " is too large")
	}
	return encoded, nil
}

// Combined UTF-8 byte length of the parts
func roomUTF8Len(parts ...string) int {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	return total
}

// Steady state reader connection with foreign keys and no journal or schema work
func hostedOpenSQLite(ctx context.Context, path string) (state.SQLiteConn, error) {
	return state.OpenDB(ctx, path, "shared-state.db", state.WithBusyTimeoutMS(10_000), state.WithWAL(false), state.WithForeignKeys())
}

// Shared root store connection that runs initialize in one IMMEDIATE transaction unless ready reports the schema current
func roomConnect(ctx context.Context, dbPath, label string, ready func(context.Context, state.SQLiteConn) (bool, error),
	initialize func(context.Context, state.SQLiteConn) error, lockRetries int) (state.SQLiteConn, error) {
	return state.OpenDB(ctx, dbPath, label, state.WithBusyTimeoutMS(10_000), state.WithForeignKeys(),
		state.WithWALLockRetries(lockRetries), state.WithInitialize(func(ctx context.Context, conn state.SQLiteConn) error {
			current, err := ready(ctx, conn)
			if err != nil || current {
				return err
			}
			if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				return err
			}
			if err := initialize(ctx, conn); err != nil {
				if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
					return rollbackErr
				}
				return err
			}
			_, err = conn.ExecContext(ctx, "COMMIT")
			return err
		}))
}

// Compare and swap UPDATE that fails with fail unless exactly one row changed
func roomFencedUpdate(ctx context.Context, conn state.SQLiteConn, query string, args []any, fail error) error {
	result, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fail
	}
	return nil
}

// Rows changed by one statement
func roomExecCount(ctx context.Context, conn state.SQLiteConn, query string, args ...any) (int64, error) {
	result, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Python int of a stored column, zero for NULL
func hrInt(row *pyjson.Object, key string) int64 {
	n, _ := pyint.Int64(row.Vals[key])
	return n
}

// Python float of a stored column, zero for NULL
func hrFloat(row *pyjson.Object, key string) float64 {
	f, _ := pyint.Float(row.Vals[key])
	return f
}

// Text of a stored column, empty for NULL
func hrStr(row *pyjson.Object, key string) string {
	text, _ := row.Vals[key].(string)
	return text
}

// Plain dict copy of one stored row
func hrDict(row *pyjson.Object) map[string]any {
	return pyjson.Plain(row).(map[string]any)
}

// Values of the named keys in order, the KeyError of the first absent one
func recordValues(record map[string]any, keys []string) ([]any, error) {
	out := make([]any, len(keys))
	for i, key := range keys {
		value, ok := record[key]
		if !ok {
			return nil, &HostedKeyError{key}
		}
		out[i] = value
	}
	return out, nil
}

// Decoded JSON text with objects as plain maps
func roomLoads(text string) (any, error) {
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	return pyjson.Plain(value), nil
}

// Whether path names a regular file
func hostedIsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Python int value of an integer that is no bool
func hostedStrictInt(value any) (int64, bool) {
	if _, isBool := value.(bool); isBool {
		return 0, false
	}
	n, ok := roomInt(value)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}
