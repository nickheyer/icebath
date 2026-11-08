package state

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Session store of one home, the hermes SessionDB
type DB = SessionDB

// Options of a database open
type Options struct {
	// Opens without creating or migrating anything
	ReadOnly bool
}

// Opens the session database at path, creating and migrating it unless read only
func Open(path string, opts Options) (*DB, error) {
	return newSessionDB(context.Background(), path, opts.ReadOnly, 2)
}

// Opens the session database of a home directory
func OpenHome(h string, opts Options) (*DB, error) {
	return newSessionDB(context.Background(), home.StatePath(h), opts.ReadOnly, 2)
}

// Runs fn in one immediate write transaction within patienceS seconds of lock contention
func (d *DB) retryWrite(fn func(tx *Tx) error, patienceS float64) error {
	ctx := d.openCtx
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		return fn(&Tx{conn: conn, ctx: ctx})
	}, &patienceS)
}

// Statements of one write transaction on the writer connection
type Tx struct {
	conn SQLiteConn
	ctx  context.Context
}

// Runs a statement in the transaction
func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(t.ctx, query, args...)
}

// Runs a query in the transaction
func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(t.ctx, query, args...)
}

// Runs a single row query in the transaction
func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(t.ctx, query, args...)
}

// Runs a write of ordinary patience
func (d *DB) write(fn func(tx *Tx) error) error {
	return d.retryWrite(fn, sessionWritePatienceS)
}

// Runs a transcript write with the longer patience
func (d *DB) transcriptWrite(fn func(tx *Tx) error) error {
	return d.retryWrite(fn, sessionTranscriptWritePatienceS)
}

// Runs an idempotent read on a pooled reader or the writer, replaying a transient disk IO error
func (d *DB) read(fn func(q querier) error) error {
	ctx := d.openCtx
	return d.readRetryingIOErr(ctx, func(conn SQLiteConn) error {
		return fn(&Tx{conn: conn, ctx: ctx})
	})
}

// Statement runner shared by read connections and transactions
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Random 32 hex character identifier
func NewUID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Session identifier carrying its creation stamp
func NewSessionID() string {
	return NewSessionIDAt(time.Time{}, DefaultHexLen)
}

// Unix seconds as a float with sub second precision
func now() float64 {
	return (*wallClock.Load())()
}

var wallClock atomic.Pointer[func() float64]

func init() {
	system := func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	wallClock.Store(&system)
}

// Installs the wall clock store timestamps read and returns the restore of the previous one
func SetWallClock(fn func() float64) func() {
	prev := wallClock.Swap(&fn)
	return func() { wallClock.Store(prev) }
}

// Values of a string list as bound parameters
func args(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
