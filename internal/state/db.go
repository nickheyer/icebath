package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite3 "modernc.org/sqlite/lib"
)

// Busy handler budget of a database opened without one
const defaultBusyTimeout = 5 * time.Second

// How a database other than the session store is opened
type DBOptions struct {
	// Name the journal mode logs give the file
	Label string
	// Busy handler budget, five seconds when zero
	BusyTimeout time.Duration
	// Leaves the journal mode the file already has instead of applying the configured one
	KeepJournal bool
	ForeignKeys bool
	// Forces synchronous FULL whatever the configured level
	SynchronousFull bool
	// Attempts of the journal mode setup against a bare database is locked, one when zero
	WALLockRetries int
	// Opens the existing file read-only with no directory creation and no journal change
	ReadOnly bool
	// Shared database settings of config.yaml
	Database Database
	// Creates or migrates the tables of the file before it is handed out
	Init func(ctx context.Context, c Conn) error
}

// One tracked connection to an icebath database other than the session store, used by one goroutine at a time
type DB struct {
	c *conn
}

// Opens path with the shared journal policy, busy budget and pragmas, closing it again when any setup step fails
func (e *Engine) OpenDB(ctx context.Context, path string, o DBOptions) (*DB, error) {
	busy := o.BusyTimeout
	if busy <= 0 {
		busy = defaultBusyTimeout
	}
	dsn := fileURI(path)
	if o.ReadOnly {
		dsn = ReadOnlyURI(path)
	} else if err := os.MkdirAll(filepath.Dir(absPath(path)), 0o777); err != nil {
		return nil, err
	}
	c, err := e.connect(ctx, dsn, path)
	if err != nil {
		return nil, presentable(err)
	}
	if err := e.setupDB(ctx, c, busy, o); err != nil {
		if cerr := c.close(); cerr != nil {
			return nil, errors.Join(presentable(err), cerr)
		}
		return nil, presentable(err)
	}
	return &DB{c: c}, nil
}

// Busy budget, journal mode, foreign keys, durability and the caller's table setup of a fresh database connection
func (e *Engine) setupDB(ctx context.Context, c *conn, busy time.Duration, o DBOptions) error {
	q := c.q()
	if _, err := q.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busy.Milliseconds())); err != nil {
		return err
	}
	if !o.ReadOnly && !o.KeepJournal {
		attempts := max(o.WALLockRetries, 1)
		for attempt := 0; ; attempt++ {
			_, err := e.applyJournalMode(ctx, q, o.Label, o.Database)
			if err == nil {
				break
			}
			if !bareLocked(err) || attempt+1 >= attempts {
				return err
			}
			if err := sleepCtx(ctx, time.Duration(10<<attempt)*time.Millisecond); err != nil {
				return err
			}
		}
	}
	if o.ForeignKeys {
		if _, err := q.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
			return err
		}
	}
	if o.SynchronousFull {
		if _, err := q.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
			return err
		}
	}
	if o.Init == nil {
		return nil
	}
	return lendTo(ctx, c, func(q Conn) error { return o.Init(ctx, q) })
}

// Reports SQLite's own bare database is locked, the one journal setup failure worth another attempt
func bareLocked(err error) bool {
	code, ok := primaryCode(err)
	return ok && code == sqlite3.SQLITE_BUSY && strings.ToLower(errText(err)) == "database is locked"
}

// Runs one statement on the connection
func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.c.q().ExecContext(ctx, query, args...)
}

// Runs one query on the connection
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.c.q().QueryContext(ctx, query, args...)
}

// Runs one single row query on the connection
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	return d.c.q().QueryRowContext(ctx, query, args...)
}

// Closes the connection and drops its tracking entry
func (d *DB) Close() error { return d.c.close() }

// Runs fn in one transaction, immediate when asked, committing on success, rolling back otherwise, and always closing the database
func (d *DB) Transact(ctx context.Context, immediate bool, fn func(c Conn) error) (err error) {
	defer func() { err = errors.Join(err, d.Close()) }()
	begin := "BEGIN"
	if immediate {
		begin = "BEGIN IMMEDIATE"
	}
	_, err = transact(ctx, d.c, begin, fn)
	return presentable(err)
}

// Runs fn in one IMMEDIATE write transaction on a connection that stays open
func (d *DB) WriteTxn(ctx context.Context, fn func(c Conn) error) error {
	_, err := transact(ctx, d.c, "BEGIN IMMEDIATE", fn)
	return presentable(err)
}

// Adds a column when missing, true when this call added it and false when a concurrent migrator already had
func AddColumnIfMissing(ctx context.Context, c Conn, table, ddl string) (bool, error) {
	_, err := c.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+ddl)
	if err == nil {
		return true, nil
	}
	if familyOf(err) == callFamily && strings.Contains(lowerText(err), "duplicate column name") {
		return false, nil
	}
	return false, err
}

// Reports the database file at least as long as its header claims, known false when that cannot be told
func FileLengthMatchesHeader(ctx context.Context, c Conn) (matches, known bool) {
	file := connectionFile(ctx, c)
	if file == "" {
		return false, false
	}
	var pages, size sql.NullInt64
	if c.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages) != nil || c.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size) != nil {
		return false, false
	}
	logical := pages.Int64 * size.Int64
	if logical == 0 {
		return false, false
	}
	fi, err := os.Stat(resolvePath(file))
	if err != nil {
		return false, false
	}
	return fi.Size() >= logical, true
}
