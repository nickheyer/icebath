package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type openDBConfig struct {
	busyTimeoutMS   int
	wal             bool
	foreignKeys     bool
	synchronousFull bool
	walLockRetries  int
	initialize      func(ctx context.Context, conn SQLiteConn) error
}

// Adjusts one setting of an OpenDB call
type OpenDBOption func(*openDBConfig)

// Sets the busy timeout passed to connect and set as the explicit pragma, 5000 by default
func WithBusyTimeoutMS(ms int) OpenDBOption { return func(c *openDBConfig) { c.busyTimeoutMS = ms } }

// Turns the WAL fallback journal setup on or off, on by default
func WithWAL(wal bool) OpenDBOption { return func(c *openDBConfig) { c.wal = wal } }

// Enables foreign key enforcement on the opened connection
func WithForeignKeys() OpenDBOption { return func(c *openDBConfig) { c.foreignKeys = true } }

// Sets synchronous FULL on the opened connection
func WithSynchronousFull() OpenDBOption { return func(c *openDBConfig) { c.synchronousFull = true } }

// Number of attempts at the journal setup when it reports database is locked, 1 by default
func WithWALLockRetries(n int) OpenDBOption { return func(c *openDBConfig) { c.walLockRetries = n } }

// Runs fn on the configured connection before OpenDB returns it, closing the connection when fn fails
func WithInitialize(fn func(ctx context.Context, conn SQLiteConn) error) OpenDBOption {
	return func(c *openDBConfig) { c.initialize = fn }
}

// Connector OpenDB resolves at call time so descriptor leak tests can track every connection it makes
var openDBConnect SQLiteOpener = ConnectSQLite

// Pause between journal setup attempts
var openDBSleep = time.Sleep

// Opens path with its parent created, applies the pragma stack and initialize, closing the connection on any failure
var OpenDB = func(ctx context.Context, path, dbLabel string, opts ...OpenDBOption) (SQLiteConn, error) {
	cfg := openDBConfig{busyTimeoutMS: 5000, wal: true, walLockRetries: 1}
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	conn, err := openDBConnect(path, SQLiteConfig{Timeout: float64(cfg.busyTimeoutMS) / 1000})
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", cfg.busyTimeoutMS)); err != nil {
		return nil, err
	}
	if cfg.wal {
		for attempt := 0; attempt < cfg.walLockRetries; attempt++ {
			_, err := ApplyWALWithFallback(ctx, conn, dbLabel, false)
			if err == nil {
				break
			}
			if !isOperationalError(err) || pystr.Lower(errText(err)) != "database is locked" || attempt+1 == cfg.walLockRetries {
				return nil, err
			}
			openDBSleep(time.Duration(float64(10*time.Millisecond) * float64(int64(1)<<attempt)))
		}
	}
	if cfg.foreignKeys {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
			return nil, err
		}
	}
	if cfg.synchronousFull {
		if _, err := conn.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
			return nil, err
		}
	}
	if cfg.initialize != nil {
		if err := cfg.initialize(ctx, conn); err != nil {
			return nil, err
		}
	}
	ok = true
	return conn, nil
}

// Ends the open transaction with COMMIT or ROLLBACK, a no op like sqlite3 commit and rollback when none is open
func endTransaction(ctx context.Context, conn SQLiteConn, verb string) error {
	_, err := conn.ExecContext(ctx, verb)
	if err != nil && errText(err) == "cannot "+pystr.Lower(verb)+" - no transaction is active" {
		return nil
	}
	return err
}

// Runs body in one transaction, joining one already open, committed on success, rolled back on error, always closing conn
func Transaction(ctx context.Context, conn SQLiteConn, immediate bool, body func(conn SQLiteConn) error) (err error) {
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			err = cerr
		}
	}()
	if _, err := sqliteAutocommit(conn); err != nil {
		return err
	}
	if immediate {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
	}
	defer func() {
		if r := recover(); r != nil {
			endTransaction(ctx, conn, "ROLLBACK")
			panic(r)
		}
	}()
	if err := body(&lazyTxConn{SQLiteConn: conn}); err != nil {
		if rerr := endTransaction(ctx, conn, "ROLLBACK"); rerr != nil {
			return rerr
		}
		return err
	}
	if cerr := endTransaction(ctx, conn, "COMMIT"); cerr != nil {
		if rerr := endTransaction(ctx, conn, "ROLLBACK"); rerr != nil {
			return rerr
		}
		return cerr
	}
	return nil
}

// Connection under the legacy sqlite3 isolation level, BEGIN issued right before a DML statement outside a transaction
type lazyTxConn struct {
	SQLiteConn
}

// Opens the implicit transaction when query is DML and the connection is in autocommit
func (c *lazyTxConn) beginBeforeDML(ctx context.Context, query string) error {
	if !isDMLStatement(query) {
		return nil
	}
	auto, err := sqliteAutocommit(c.SQLiteConn)
	if err != nil || !auto {
		return err
	}
	_, err = c.SQLiteConn.ExecContext(ctx, "BEGIN ")
	return err
}

func (c *lazyTxConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := c.beginBeforeDML(ctx, query); err != nil {
		return nil, err
	}
	return c.SQLiteConn.ExecContext(ctx, query, args...)
}

func (c *lazyTxConn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := c.beginBeforeDML(ctx, query); err != nil {
		return nil, err
	}
	return c.SQLiteConn.QueryContext(ctx, query, args...)
}

// Row of query, or the row of the implicit BEGIN carrying its error when that BEGIN fails
func (c *lazyTxConn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if isDMLStatement(query) {
		auto, err := sqliteAutocommit(c.SQLiteConn)
		if err == nil && auto {
			begin := c.SQLiteConn.QueryRowContext(ctx, "BEGIN ")
			if begin.Err() != nil {
				return begin
			}
			begin.Scan()
		}
	}
	return c.SQLiteConn.QueryRowContext(ctx, query, args...)
}

// Statement text after leading whitespace and comments, empty when nothing else remains, as CPython lstrip_sql
func lstripSQL(query string) string {
	for pos := 0; pos < len(query); pos++ {
		switch query[pos] {
		case ' ', '\t', '\f', '\n', '\r':
		case '-':
			if pos+1 < len(query) && query[pos+1] == '-' {
				end := strings.IndexByte(query[pos+2:], '\n')
				if end < 0 {
					return ""
				}
				pos += 2 + end
				continue
			}
			return query[pos:]
		case '/':
			if pos+1 < len(query) && query[pos+1] == '*' {
				end := strings.Index(query[pos+2:], "*/")
				if end < 0 {
					return ""
				}
				pos += 2 + end + 1
				continue
			}
			return query[pos:]
		default:
			return query[pos:]
		}
	}
	return ""
}

// Reports whether CPython sqlite3 begins a transaction before the statement, INSERT UPDATE DELETE or REPLACE
func isDMLStatement(query string) bool {
	tail := lstripSQL(query)
	for _, verb := range []string{"insert", "update", "delete", "replace"} {
		if len(tail) >= len(verb) && asciiLower(tail[:len(verb)]) == verb {
			return true
		}
	}
	return false
}

// Adds a column through ALTER TABLE, true when this call added it and false when a concurrent migrator already had
func AddColumnIfMissing(ctx context.Context, conn SQLiteConn, table, column, ddl string) (bool, error) {
	_, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, ddl))
	if err == nil {
		return true, nil
	}
	if isOperationalError(err) && strings.Contains(pystr.Lower(errText(err)), "duplicate column name") {
		return false, nil
	}
	return false, err
}

// Runs body in an IMMEDIATE transaction on a long lived connection, a failed ROLLBACK never hiding the body error
func WriteTxn(ctx context.Context, conn SQLiteConn, body func() error) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := body(); err != nil {
		if _, rerr := conn.ExecContext(ctx, "ROLLBACK"); rerr != nil && !isOperationalError(rerr) {
			return rerr
		}
		return err
	}
	_, err := conn.ExecContext(ctx, "COMMIT")
	return err
}

// Lowercases ASCII letters only, the PyOS_strnicmp comparison
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
