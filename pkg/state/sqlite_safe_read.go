package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
)

// Reports a raw file operation attempted on a database with live connections
type LiveConnectionError struct {
	Path string
	Main string
	What string
}

func (e *LiveConnectionError) Error() string {
	subject := "it"
	if e.Main != e.Path {
		subject = "its main database " + e.Main
	}
	return fmt.Sprintf("Refusing to %s %s: a connection to %s is still open in this process, and raw file access would cancel that connection's POSIX advisory locks. Close all database handles (stop the gateway/dashboard) and retry.", e.What, e.Path, subject)
}

// Guards the live registry together with the connection lifecycle syscalls it describes
var liveMu sync.Mutex
var liveConnections = map[string]int{}

// Opens the raw descriptor of a header probe, a seam for tests that pause inside the read
var safeReadOpen = os.Open

// Canonical filesystem key of a database path
func liveKey(path string) string {
	return realpathLax(path)
}

// Adjusts the live count of an already canonical key, caller holds liveMu
func trackKey(key string, delta int) {
	remaining := liveConnections[key] + delta
	if remaining > 0 {
		liveConnections[key] = remaining
	} else {
		delete(liveConnections, key)
	}
}

// Records that this process holds one more connection to the path
func TrackConnection(path string) {
	liveMu.Lock()
	defer liveMu.Unlock()
	trackKey(liveKey(path), 1)
}

// Records that one connection to the path was closed
func UntrackConnection(path string) {
	liveMu.Lock()
	defer liveMu.Unlock()
	trackKey(liveKey(path), -1)
}

// The tracked main database key that makes this key live, or empty
func liveMainKey(key string) string {
	if _, ok := liveConnections[key]; ok {
		return key
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if strings.HasSuffix(key, suffix) {
			main := strings.TrimSuffix(key, suffix)
			if _, ok := liveConnections[main]; ok {
				return main
			}
		}
	}
	return ""
}

// Reports whether this process holds a connection to the path or its main database
func HasLiveConnection(path string) bool {
	liveMu.Lock()
	defer liveMu.Unlock()
	return liveMainKey(liveKey(path)) != ""
}

// SQLite connection surface the tracked helpers work over, satisfied by *sql.Conn and *sql.DB
type SQLiteConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	Close() error
}

// Connection that releases its live registry entry exactly once, only after its descriptor is closed
type TrackedConn struct {
	SQLiteConn
	trackedPath string
}

// Closes the connection and untracks its path together under the lifecycle lock, keeping the entry when close fails
func (c *TrackedConn) Close() error {
	liveMu.Lock()
	defer liveMu.Unlock()
	path := c.trackedPath
	if err := c.SQLiteConn.Close(); err != nil {
		return err
	}
	if path != "" {
		c.trackedPath = ""
		trackKey(liveKey(path), -1)
	}
	return nil
}

// The sqlite3.connect arguments an opener honours, busy timeout in seconds and URI filename parsing
type SQLiteConfig struct {
	Timeout float64
	URI     bool
}

// Opens one SQLite connection to a database name, the role sqlite3.connect plays
type SQLiteOpener func(database string, cfg SQLiteConfig) (SQLiteConn, error)

type trackedOptions struct {
	trackingPath    string
	hasTrackingPath bool
	connectFn       SQLiteOpener
	cfg             SQLiteConfig
}

// Option of ConnectTracked
type TrackedOption func(*trackedOptions)

// Tracks the connection under this filesystem path instead of the one SQLite reports
func WithTrackingPath(path string) TrackedOption {
	return func(o *trackedOptions) { o.trackingPath, o.hasTrackingPath = path, true }
}

// Opens through fn instead of ConnectSQLite
func WithConnectFn(fn SQLiteOpener) TrackedOption {
	return func(o *trackedOptions) { o.connectFn = fn }
}

// Busy timeout in seconds handed to the opener, five when unset
func WithTimeout(seconds float64) TrackedOption {
	return func(o *trackedOptions) { o.cfg.Timeout = seconds }
}

// Parses a database name starting with file as an SQLite URI
func WithURI() TrackedOption {
	return func(o *trackedOptions) { o.cfg.URI = true }
}

// One connection in its own single slot pool, closing both like a sqlite3 Connection close
type sqliteConn struct {
	*sql.Conn
	pool *sql.DB
}

// Closes the connection and its pool, a second close returning nil
func (c *sqliteConn) Close() error {
	err := c.Conn.Close()
	if errors.Is(err, sql.ErrConnDone) {
		err = nil
	}
	if perr := c.pool.Close(); err == nil {
		err = perr
	}
	return err
}

// File URI naming a literal filename so a question mark or a file prefix keeps its plain meaning
func sqliteFilenameURI(name string) string {
	if filepath.IsAbs(name) {
		return pathAsURI(name)
	}
	return "file:" + urlQuoteBytes(filepath.ToSlash(name))
}

// sqlite3.connect opening one autocommit connection right away with its busy timeout set
func ConnectSQLite(database string, cfg SQLiteConfig) (SQLiteConn, error) {
	dsn := database
	isFileURI := strings.HasPrefix(database, "file:")
	if (isFileURI && !cfg.URI) || (!isFileURI && strings.IndexByte(database, '?') >= 1) {
		dsn = sqliteFilenameURI(database)
	}
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(1)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		pool.Close()
		return nil, err
	}
	c := &sqliteConn{Conn: conn, pool: pool}
	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout = %d", int64(cfg.Timeout*1000))); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Opens a connection and registers it for the lifetime of its descriptor, open and registration under one lock
func ConnectTracked(path string, opts ...TrackedOption) (*TrackedConn, error) {
	o := trackedOptions{cfg: SQLiteConfig{Timeout: 5.0}}
	for _, opt := range opts {
		opt(&o)
	}
	opener := o.connectFn
	if opener == nil {
		opener = ConnectSQLite
	}
	liveMu.Lock()
	defer liveMu.Unlock()
	conn, err := opener(path, o.cfg)
	if err != nil {
		return nil, err
	}
	resolved, ok := "", false
	if o.hasTrackingPath {
		resolved, ok = liveKey(o.trackingPath), true
	} else {
		if isNilConn(conn) {
			return nil, &pyConvError{"AttributeError", "'NoneType' object has no attribute 'execute'"}
		}
		resolved, ok = canonicalDBPath(conn)
	}
	if !ok {
		if tracked, isTracked := conn.(*TrackedConn); isTracked {
			return tracked, nil
		}
		return &TrackedConn{SQLiteConn: conn}, nil
	}
	tracked, err := retrofitTracking(conn, resolved)
	if err != nil {
		return nil, err
	}
	tracked.trackedPath = resolved
	trackKey(resolved, 1)
	return tracked, nil
}

// Reports a connection to a probe-able database that cannot carry its tracking entry, refused rather than returned untracked
type UntrackableConnectionError struct {
	Path    string
	Factory string
	Cause   error
}

func (e *UntrackableConnectionError) Error() string {
	return fmt.Sprintf("connection to %s uses factory %s, which "+
		"cannot release its tracking entry on close; byte-probe safety "+
		"for this database would be silently lost", e.Path, e.Factory)
}

func (e *UntrackableConnectionError) Unwrap() error { return e.Cause }

// Whether an opener handed back no connection at all, the None a Python opener could return
func isNilConn(conn SQLiteConn) bool {
	if conn == nil {
		return true
	}
	v := reflect.ValueOf(conn)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// Gives a connection from any opener the untrack on close behaviour, refusing a missing connection that cannot carry it
func retrofitTracking(conn SQLiteConn, resolved string) (*TrackedConn, error) {
	if isNilConn(conn) {
		return nil, &UntrackableConnectionError{Path: resolved, Factory: "NoneType",
			Cause: &pyConvError{"TypeError", "__class__ assignment only supported for mutable types or ModuleType subclasses"}}
	}
	if tracked, ok := conn.(*TrackedConn); ok {
		return tracked, nil
	}
	return &TrackedConn{SQLiteConn: conn}, nil
}

// On disk path of main as SQLite reports it, false for in memory or unnamed databases and SQLite errors
func canonicalDBPath(conn SQLiteConn) (string, bool) {
	var seq, name any
	var file sql.NullString
	if err := conn.QueryRowContext(context.Background(), "PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
		return "", false
	}
	if !file.Valid || file.String == "" {
		return "", false
	}
	return liveKey(file.String), true
}

// First column of the first row of a pragma, with the error text Python's fetchone()[0] raises
func pragmaFirstValue(conn SQLiteConn, pragma string) (any, error) {
	rows, err := conn.QueryContext(context.Background(), pragma)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, &pyConvError{"TypeError", "'NoneType' object is not subscriptable"}
	}
	if len(cols) == 0 {
		return nil, &pyConvError{"IndexError", "tuple index out of range"}
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	if b, ok := vals[0].([]byte); ok {
		for _, c := range b {
			if c >= 0x80 {
				return nil, &pyConvError{"ValueError", "invalid literal for int() with base 10: " + pyBytesRepr(b)}
			}
		}
		return string(b), nil
	}
	return vals[0], nil
}

// Logical database size page_count times page_size read over the connection, false when the pragmas fail
func PageCountBytes(conn SQLiteConn) (int64, bool) {
	var product *big.Int
	err := func() error {
		pageCount, err := pragmaFirstValue(conn, "PRAGMA page_count")
		if err != nil {
			return err
		}
		pageSize, err := pragmaFirstValue(conn, "PRAGMA page_size")
		if err != nil {
			return err
		}
		count, err := pyIntOf(pageCount)
		if err != nil {
			return err
		}
		size, err := pyIntOf(pageSize)
		if err != nil {
			return err
		}
		product = new(big.Int).Mul(count, size)
		return nil
	}()
	if err != nil {
		slog.Debug("page_count/page_size unavailable: " + errText(err))
		return 0, false
	}
	return product.Int64(), true
}

// Whether the file is at least as long as the header claims, false in ok when the check is inconclusive
func FileLengthMatchesHeader(conn SQLiteConn) (matches bool, ok bool) {
	path, ok := canonicalDBPath(conn)
	if !ok {
		return false, false
	}
	logical, ok := PageCountBytes(conn)
	if !ok || logical == 0 {
		return false, false
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, false
	}
	return st.Size() >= logical, true
}

// Runs fn under the connection lifecycle lock, refusing while the database is live
func OfflineFileAccess(path, what string, fn func()) error {
	liveMu.Lock()
	defer liveMu.Unlock()
	key := liveKey(path)
	if main := liveMainKey(key); main != "" {
		if what == "" {
			what = "read"
		}
		return &LiveConnectionError{Path: path, Main: main, What: what}
	}
	fn()
	return nil
}

// Reads the leading bytes of a database file only when no connection is live, a negative length reading it all
func ReadHeaderBytesPreopen(path string, length int, force bool) ([]byte, bool) {
	liveMu.Lock()
	defer liveMu.Unlock()
	if !force && liveMainKey(liveKey(path)) != "" {
		slog.Debug(fmt.Sprintf("refusing byte-level read of %s: a live connection exists in "+
			"this process and close() would cancel its POSIX locks", path))
		return nil, false
	}
	f, err := safeReadOpen(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.IsDir() {
		return nil, false
	}
	if length < 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, false
		}
		return data, true
	}
	buf := make([]byte, length)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, false
	}
	return buf[:n], true
}
