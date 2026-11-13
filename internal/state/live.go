package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Error for raw file access to a database this process still holds a connection to
var ErrLiveConnection = errors.New("database has a live connection in this process")

// Refusal of raw file access worded for the operator, matching ErrLiveConnection
type liveRefusal struct{ msg string }

// Operator text
func (r *liveRefusal) Error() string { return r.msg }

// Class every live connection refusal belongs to
func (r *liveRefusal) Unwrap() error { return ErrLiveConnection }

// Live connections this process holds per resolved database path, the lock also covering every open
type liveRegistry struct {
	mu    sync.Mutex
	count map[string]int
}

// Tracked key that makes key live, the main database for a sidecar, false when none is (caller holds mu)
func (l *liveRegistry) mainKey(key string) (string, bool) {
	if l.count[key] > 0 {
		return key, true
	}
	for _, suffix := range []string{walSuffix, shmSuffix} {
		if main, ok := strings.CutSuffix(key, suffix); ok && l.count[main] > 0 {
			return main, true
		}
	}
	return "", false
}

// Reports whether this process holds a connection to path or to the database path is a sidecar of
func (e *Engine) HasLiveConnection(path string) bool {
	key := resolvePath(path)
	e.live.mu.Lock()
	defer e.live.mu.Unlock()
	_, live := e.live.mainKey(key)
	return live
}

// First n bytes of path read raw, refused while a connection is live unless force is set, false when unreadable
func (e *Engine) ReadHeader(path string, n int, force bool) ([]byte, bool) {
	head, err := e.readHeader(path, n, force)
	return head, err == nil
}

// Reads until buf is full or the file ends, an early end of file not counting as an error
func readFull(f *os.File, buf []byte) (int, error) {
	got, err := io.ReadFull(f, buf)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return got, nil
	}
	return got, err
}

// Runs fn as raw file access to path while no connection can open, refusing when one is live
func (e *Engine) OfflineAccess(path, what string, fn func() error) error {
	key := resolvePath(path)
	e.live.mu.Lock()
	defer e.live.mu.Unlock()
	if main, live := e.live.mainKey(key); live {
		subject := "it"
		if main != key {
			subject = "its main database " + main
		}
		return &liveRefusal{msg: fmt.Sprintf("Refusing to %s %s: a connection to %s is still open in this process, "+
			"and raw file access would cancel that connection's POSIX advisory locks. Close all database handles "+
			"(stop the gateway/dashboard) and retry.", what, path, subject)}
	}
	return fn()
}

// One physical SQLite connection pinned for its whole life and tracked while open
type conn struct {
	mu   sync.Mutex
	db   *sql.DB
	sc   *sql.Conn
	key  string
	live *liveRegistry
}

// Error of a driver whose connections no longer expose the SQLite handle the engine guards transactions through
var errNoHandle = errors.New("SQLite driver connection exposes no database handle")

// Opens dsn as one physical connection tracked under the resolved trackPath, open and registration under one lock
func (e *Engine) connect(ctx context.Context, dsn, trackPath string) (*conn, error) {
	connector, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	key := resolvePath(trackPath)
	e.live.mu.Lock()
	defer e.live.mu.Unlock()
	sc, err := db.Conn(ctx)
	if err == nil {
		if err = sc.Raw(func(dc any) error { _, _, herr := driverHandle(dc); return herr }); err != nil {
			err = errors.Join(err, sc.Close())
		}
	}
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	e.live.count[key]++
	return &conn{db: db, sc: sc, key: key, live: &e.live}, nil
}

// Closes the connection and drops its tracking entry only once the descriptor is gone
func (c *conn) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil
	}
	if c.sc != nil {
		releaseErr := c.sc.Close()
		c.sc = nil
		if releaseErr != nil {
			return releaseErr
		}
	}
	if err := c.db.Close(); err != nil {
		return err
	}
	c.db = nil
	c.live.mu.Lock()
	if c.live.count[c.key]--; c.live.count[c.key] <= 0 {
		delete(c.live.count, c.key)
	}
	c.live.mu.Unlock()
	return nil
}

// SQLite handle and thread state the driver keeps in its connection, found by field name and type
func driverHandle(dc any) (*libc.TLS, uintptr, error) {
	v := reflect.ValueOf(dc)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return nil, 0, fmt.Errorf("%w: %T", errNoHandle, dc)
	}
	db, tls := v.Elem().FieldByName("db"), v.Elem().FieldByName("tls")
	if !db.IsValid() || db.Kind() != reflect.Uintptr || !tls.IsValid() || tls.Type() != reflect.TypeFor[*libc.TLS]() {
		return nil, 0, fmt.Errorf("%w: %T", errNoHandle, dc)
	}
	t := *(**libc.TLS)(unsafe.Pointer(tls.UnsafeAddr()))
	if t == nil || db.Uint() == 0 {
		return nil, 0, fmt.Errorf("%w: %T is closed", errNoHandle, dc)
	}
	return t, uintptr(db.Uint()), nil
}

// Pinned connection statements run on, ErrClosed once it was closed
func (c *conn) open() (*sql.Conn, error) {
	if c.sc == nil {
		return nil, fmt.Errorf("%w: connection to %s", ErrClosed, c.key)
	}
	return c.sc, nil
}

// Runs fn on the SQLite handle while database/sql holds the connection for this goroutine alone
func (c *conn) raw(fn func(tls *libc.TLS, db uintptr) error) error {
	sc, err := c.open()
	if err != nil {
		return err
	}
	return sc.Raw(func(dc any) error {
		tls, db, err := driverHandle(dc)
		if err != nil {
			return err
		}
		return fn(tls, db)
	})
}

// Reports whether a transaction is open on the connection
func (c *conn) inTransaction() (bool, error) {
	open := false
	err := c.raw(func(tls *libc.TLS, db uintptr) error {
		open = sqlite3.Xsqlite3_get_autocommit(tls, db) == 0
		return nil
	})
	return open, err
}

// Switches off the checkpoint and sidecar unlink SQLite runs when the last connection to a file closes
func (c *conn) disableCloseCheckpoint() error {
	return c.raw(func(tls *libc.TLS, db uintptr) error {
		va := tls.Alloc(16)
		defer tls.Free(16)
		libc.VaList(va, int32(1), uintptr(0))
		if rc := sqlite3.Xsqlite3_db_config(tls, db, sqlite3.SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE, va); rc != sqlite3.SQLITE_OK {
			return fmt.Errorf("sqlite3_db_config(SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE) returned %d", rc)
		}
		return nil
	})
}

// Statement runner of the connection for engine work that closes every result it opens
func (c *conn) q() Conn { return &runner{c: c} }

// Statement runner lent to one callback, a write runner keeping every statement inside the write transaction
func (c *conn) lend(write bool) *runner { return &runner{c: c, write: write} }

// Error of a write whose transaction SQLite rolled back after a statement inside it failed
var errTxnRolledBack = callErr(nil, "SQLite rolled back the write transaction after a statement inside it failed; nothing was committed")

// Statements on one lent connection with the result sets they opened
type runner struct {
	c     *conn
	write bool
	mu    sync.Mutex
	rows  []*sql.Rows
	// SQLite ended the write transaction early and later statements ran inside one that never commits
	contained bool
}

// Keeps a write callback inside a transaction, opening one that never commits when SQLite ended the real one
func (r *runner) hold(ctx context.Context) error {
	if !r.write {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	open, err := r.c.inTransaction()
	if err != nil || open {
		return err
	}
	if _, err := r.c.sc.ExecContext(context.WithoutCancel(ctx), "BEGIN"); err != nil {
		return err
	}
	r.contained = true
	return nil
}

// Runs one statement
func (r *runner) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	sc, err := r.c.open()
	if err != nil {
		return nil, err
	}
	if err := r.hold(ctx); err != nil {
		return nil, err
	}
	res, err := sc.ExecContext(ctx, query, args...)
	return res, presentable(err)
}

// Runs one query, its result closed at settle when the callback leaves it open
func (r *runner) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	sc, err := r.c.open()
	if err != nil {
		return nil, err
	}
	if err := r.hold(ctx); err != nil {
		return nil, err
	}
	rows, err := sc.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, presentable(err)
	}
	r.mu.Lock()
	r.rows = append(r.rows, rows)
	r.mu.Unlock()
	return rows, nil
}

// Runs one query for its first row
func (r *runner) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	rows, err := r.QueryContext(ctx, query, args...)
	return &Row{rows: rows, err: err}
}

// Closes every result set the callback left open, ending the read snapshot they pinned
func (r *runner) closeRows() {
	r.mu.Lock()
	rows := r.rows
	r.rows = nil
	r.mu.Unlock()
	for _, rs := range rows {
		_ = rs.Close()
	}
}

// Error of a write runner that cannot commit, SQLite having ended its transaction during the callback
func (r *runner) lostTransaction() error {
	r.mu.Lock()
	contained := r.contained
	r.mu.Unlock()
	if contained {
		return errTxnRolledBack
	}
	open, err := r.c.inTransaction()
	if err != nil {
		return err
	}
	if !open {
		return errTxnRolledBack
	}
	return nil
}

// Returns a read connection to autocommit, closing left open results and rolling back a transaction the callback began
func (r *runner) settle(ctx context.Context) error {
	r.closeRows()
	open, err := r.c.inTransaction()
	if err != nil || !open {
		return err
	}
	if _, err := r.c.sc.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); err != nil {
		return err
	}
	if open, err = r.c.inTransaction(); err == nil && open {
		return callErr(nil, "a read left a transaction open on its connection that ROLLBACK did not end")
	}
	return err
}
