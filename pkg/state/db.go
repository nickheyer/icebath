package state

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Patience of an ordinary write before the turn fails on contention
const WritePatience = 20 * time.Second

// Patience of a transcript write, whose failure aborts the turn
const TranscriptWritePatience = 60 * time.Second

// Writes between passive WAL checkpoints
const checkpointEveryWrites = 50

// Options of a database open
type Options struct {
	// wal or delete, wal when empty
	JournalMode string
	// Pages between automatic checkpoints, zero for the SQLite default
	WALAutocheckpoint int
	// Bytes the WAL may keep after a checkpoint, zero for the SQLite default
	JournalSizeLimit int
	// Opens without creating or migrating anything
	ReadOnly bool
}

// Session store of one home
type DB struct {
	path     string
	readOnly bool
	db       *sql.DB
	writeMu  sync.Mutex
	writes   int
	closed   bool
	closeMu  sync.Mutex
	// Opened through Acquire, so Close releases one registry reference instead of closing
	sharedRegistryOwned atomic.Bool
}

// Error of a write that stayed locked past its patience
type LockedError struct {
	Path string
	Err  error
}

func (e *LockedError) Error() string {
	return "state database " + e.Path + " stayed locked: " + e.Err.Error()
}

func (e *LockedError) Unwrap() error { return e.Err }

// Reports whether an error is SQLite lock contention
func isLockError(err error) bool {
	return IsSQLiteLockError(err) || isNoMoreRows(err)
}

// Opens the session database of a home, creating and migrating it unless read only
func Open(path string, opts Options) (*DB, error) {
	if !opts.ReadOnly {
		if _, err := home.Mkdir(filepath.Dir(path)); err != nil {
			return nil, err
		}
	} else if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	mode := strings.ToLower(strings.TrimSpace(opts.JournalMode))
	if mode == "" {
		mode = "wal"
	}
	pragmas := []string{"busy_timeout(1000)", "foreign_keys(1)", "synchronous(NORMAL)"}
	if !opts.ReadOnly {
		pragmas = append(pragmas, "journal_mode("+mode+")")
	}
	if opts.WALAutocheckpoint > 0 {
		pragmas = append(pragmas, fmt.Sprintf("wal_autocheckpoint(%d)", opts.WALAutocheckpoint))
	}
	if opts.JournalSizeLimit > 0 {
		pragmas = append(pragmas, fmt.Sprintf("journal_size_limit(%d)", opts.JournalSizeLimit))
	}
	dsn := "file:" + path + "?"
	var params []string
	for _, p := range pragmas {
		params = append(params, "_pragma="+p)
	}
	if opts.ReadOnly {
		params = append(params, "mode=ro")
	}
	dsn += strings.Join(params, "&")
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	d := &DB{path: path, readOnly: opts.ReadOnly, db: sqlDB}
	TrackConnection(path)
	if !opts.ReadOnly {
		if err := d.initSchema(); err != nil {
			sqlDB.Close()
			UntrackConnection(path)
			return nil, err
		}
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if st, err := os.Stat(p); err == nil && st.Mode().Perm()&0o077 != 0 {
				_ = os.Chmod(p, 0o600)
			}
		}
	}
	registerTestInstance(d)
	return d, nil
}

// Opens the session database of a home directory
func OpenHome(h string, opts Options) (*DB, error) {
	return Open(home.StatePath(h), opts)
}

// Path of the database file
func (d *DB) Path() string { return d.path }

// Closes the database after a passive checkpoint, or releases one reference of a registry shared handle
func (d *DB) Close() error {
	if d.sharedRegistryOwned.Load() {
		Release(d)
		return nil
	}
	d.closeMu.Lock()
	defer d.closeMu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	defer UntrackConnection(d.path)
	if !d.readOnly {
		d.writeMu.Lock()
		_, _ = d.db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
		d.writeMu.Unlock()
	}
	return d.db.Close()
}

// Applies the declared schema, adds columns the file lacks and stamps the version
func (d *DB) initSchema() error {
	return d.retryWrite(func(tx *Tx) error {
		if _, err := tx.Exec(schemaSQL); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := reconcileColumns(tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ftsSQL); err != nil {
			return fmt.Errorf("apply full text schema: %w", err)
		}
		var version int
		err := tx.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", goSchemaVersion); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')"); err != nil {
				return fmt.Errorf("seed full text index: %w", err)
			}
		case err != nil:
			return err
		case version < goSchemaVersion:
			if _, err := tx.Exec("UPDATE schema_version SET version = ?", goSchemaVersion); err != nil {
				return err
			}
		}
		return nil
	}, WritePatience)
}

var createTableRe = regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\);`)

// Columns of every declared table as name to definition
func declaredColumns() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, m := range createTableRe.FindAllStringSubmatch(schemaSQL, -1) {
		table, body := m[1], m[2]
		cols := map[string]string{}
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
			if line == "" || strings.HasPrefix(line, "--") {
				continue
			}
			upper := strings.ToUpper(line)
			if strings.HasPrefix(upper, "FOREIGN KEY") || strings.HasPrefix(upper, "PRIMARY KEY") || strings.HasPrefix(upper, "UNIQUE") || strings.HasPrefix(upper, "CHECK") {
				continue
			}
			name, def, _ := strings.Cut(line, " ")
			cols[name] = def
		}
		out[table] = cols
	}
	return out
}

// Adds declared columns a legacy table lacks
func reconcileColumns(tx *Tx) error {
	for table, cols := range declaredColumns() {
		rows, err := tx.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, table))
		if err != nil {
			return err
		}
		live := map[string]bool{}
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				rows.Close()
				return err
			}
			live[name] = true
		}
		rows.Close()
		if len(live) == 0 {
			continue
		}
		for name, def := range cols {
			if live[name] {
				continue
			}
			def = strings.Replace(strings.Replace(def, "PRIMARY KEY AUTOINCREMENT", "", 1), "PRIMARY KEY", "", 1)
			if strings.Contains(strings.ToUpper(def), "NOT NULL") && !strings.Contains(strings.ToUpper(def), "DEFAULT") {
				def = strings.Replace(def, "NOT NULL", "", 1)
			}
			if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN "%s" %s`, table, name, def)); err != nil {
				return fmt.Errorf("add column %s.%s: %w", table, name, err)
			}
		}
	}
	return nil
}

// Runs fn in one immediate transaction, retrying lock contention with jitter until patience runs out
func (d *DB) retryWrite(fn func(tx *Tx) error, patience time.Duration) error {
	if d.readOnly {
		return errors.New("state database opened read only")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	deadline := time.Now().Add(patience)
	start := time.Now()
	for {
		err := d.immediate(fn)
		if err == nil {
			d.writes++
			if d.writes%checkpointEveryWrites == 0 {
				_, _ = d.db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
			}
			return nil
		}
		if !isLockError(err) {
			return err
		}
		if time.Now().After(deadline) {
			return &LockedError{Path: d.path, Err: err}
		}
		var wait time.Duration
		if time.Since(start) < 2*time.Second {
			wait = time.Duration(20+rand.IntN(131)) * time.Millisecond
		} else {
			wait = time.Duration(250+rand.IntN(751)) * time.Millisecond
		}
		time.Sleep(wait)
	}
}

// Statements of one write transaction bound to a single connection
type Tx struct {
	conn *sql.Conn
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

// One BEGIN IMMEDIATE transaction around fn
func (d *DB) immediate(fn func(tx *Tx) error) error {
	ctx := context.Background()
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	tx := &Tx{conn: conn, ctx: ctx}
	if err := fn(tx); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	return nil
}

// Runs a write of ordinary patience
func (d *DB) write(fn func(tx *Tx) error) error {
	return d.retryWrite(fn, WritePatience)
}

// Runs a transcript write with the longer patience
func (d *DB) transcriptWrite(fn func(tx *Tx) error) error {
	return d.retryWrite(fn, TranscriptWritePatience)
}

// Reads one row through fn, retrying lock contention briefly
func (d *DB) read(fn func(q querier) error) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := fn(d.db)
		if err == nil || !isLockError(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Duration(20+rand.IntN(131)) * time.Millisecond)
	}
}

// Statement runner shared by connections and transactions
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

// SHA-256 hex of a prompt
func promptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// Unix seconds as a float with sub second precision
func now() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// Values of a string list as bound parameters
func args(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Value of a metadata key, empty when unset
func (d *DB) GetMeta(key string) (string, bool, error) {
	var value sql.NullString
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT value FROM state_meta WHERE key = ?", key).Scan(&value)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value.String, value.Valid, nil
}

// Writes a metadata key
func (d *DB) SetMeta(key, value string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO state_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
		return err
	})
}

// Metadata keys with a prefix and their values
func (d *DB) ListMetaPrefix(prefix string) (map[string]string, error) {
	out := map[string]string{}
	err := d.read(func(q querier) error {
		rows, err := q.Query("SELECT key, value FROM state_meta WHERE key LIKE ? ESCAPE '\\' ORDER BY key", EscapeLike(prefix)+"%")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var v sql.NullString
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			out[k] = v.String
		}
		return rows.Err()
	})
	return out, err
}
