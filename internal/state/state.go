// SQLite storage engine opening, guarding, reading and writing state.db and every other icebath database
package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Write patience budgets, routine writes, transcript writes that abort a turn, and observation-only activity writes
const (
	WritePatience           = 20 * time.Second
	TranscriptWritePatience = 60 * time.Second
	ActivityWritePatience   = 500 * time.Millisecond
)

// Jittered sleeps between write retries, fast for the first two seconds of a budget and slow afterwards
const (
	writeRetryMin     = 20 * time.Millisecond
	writeRetryMax     = 150 * time.Millisecond
	writeRetrySlowAt  = 2 * time.Second
	writeRetrySlowMin = 250 * time.Millisecond
	writeRetrySlowMax = time.Second
)

// Wait a write gives a live compression lock before the refusal stands
const compressionBusyWait = 5 * time.Second

// Successful writes between passive WAL checkpoints and between incremental full-text merges
const (
	checkpointEveryWrites = 50
	ftsMergeEveryWrites   = 1000
)

// Busy handler budgets of the writer connection and of every read connection
const (
	writerBusyTimeout = time.Second
	readBusyTimeout   = 5 * time.Second
)

// Read-only open retries of a transient disk I/O error and the pause between them
const (
	readIOErrRetries = 3
	readIOErrBackoff = 50 * time.Millisecond
)

// Back-off after a failed read connection open before the pool tries another
const readOpenRetryAfter = 60 * time.Second

// Statements a callback runs on the one connection the engine lends it
type Conn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *Row
}

// First row of a query, or the error that stopped the query before it ran
type Row struct {
	rows *sql.Rows
	err  error
}

// Copies the first row into dest and closes the result, sql.ErrNoRows when there is none
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	for _, d := range dest {
		if _, ok := d.(*sql.RawBytes); ok {
			return errors.New("sql.RawBytes cannot outlive the row it points into")
		}
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return presentable(err)
		}
		return sql.ErrNoRows
	}
	if err := r.rows.Scan(dest...); err != nil {
		return presentable(err)
	}
	return presentable(r.rows.Close())
}
