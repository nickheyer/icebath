package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	rollbackStepSeconds     = 0.1
	rollbackFirstStepPages  = 256
	rollbackBusySleep       = 10 * time.Millisecond
	rollbackMaxRestarts     = 20
	rollbackLockHoldSeconds = 0.25
	abandonedStagingSeconds = 2 * 3600
	emergencyPrefix         = "state.db.pre-update-emergency-"
)

// RuntimeError raised by the backup code with its message
type runtimeError struct{ msg string }

func (e *runtimeError) Error() string { return e.msg }

// RuntimeError raised when a snapshot source stays busy past its deadline
type sqliteBackupTimeout struct{ msg string }

func (e *sqliteBackupTimeout) Error() string { return e.msg }

func (e *sqliteBackupTimeout) Unwrap() error { return &runtimeError{e.msg} }

// Interruption of a copy by a stop request, passing every handler that catches ordinary failures
type Interrupted struct{ Cause error }

func (e *Interrupted) Error() string { return "interrupted: " + e.Cause.Error() }

func (e *Interrupted) Unwrap() error { return e.Cause }

var errBackupRestarted = errors.New("a foreign write restarted the stepped snapshot from page one")

// Online backup of one connection into another, a seam tests wrap to act between steps
var connBackup = pysqlite.Backup

// Clock of the copy deadlines and step timings, a seam tests drive
var monotonic = time.Now

// File removal of a failed copy destination, a seam tests observe
var removeFile = os.Remove

// Single connection to one SQLite database with its pool
type sqliteConn struct {
	db *sql.DB
	c  *sql.Conn
}

// Raw driver access for the backup helpers
func (s *sqliteConn) Raw(f func(driverConn any) error) error { return s.c.Raw(f) }

// Opens one connection to a modernc DSN, connecting at once like sqlite3.connect
func openSQLite(ctx context.Context, dsn string) (*sqliteConn, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	c, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &sqliteConn{db: db, c: c}, nil
}

// Opens a database file as sqlite3.connect of its str path does, with the five second busy timeout
func connectPath(ctx context.Context, path string) (*sqliteConn, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri, err := pypath.AsURI(abs)
	if err != nil {
		return nil, err
	}
	return openSQLite(ctx, uri+"?_pragma=busy_timeout(5000)")
}

// Closes a connection and its pool, swallowing every error
func closeQuietly(c *sqliteConn) {
	if c != nil {
		c.c.Close()
		c.db.Close()
	}
}

// str of an exception value as Python prints it
func excText(err error) string {
	var re *runtimeError
	if errors.As(err, &re) {
		return re.msg
	}
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return state.ErrText(err)
}

// Final traceback line of an uncaught exception, its class then its text
func excLine(err error) string {
	class := "Exception"
	var re *runtimeError
	var pe *pysqlite.Error
	switch {
	case errors.As(err, &re):
		class = "RuntimeError"
	case errors.As(err, &pe):
		class = "sqlite3." + pe.PyTypeName()
	case pypath.IsOSError(err):
		class = pypath.OSErrorClass(err)
	default:
		if c, ok := state.SQLiteClassOf(err); ok {
			class = "sqlite3." + sqliteClassName(c)
		}
	}
	return class + ": " + excText(err)
}

// Python class name of a state sqlite3 exception class
func sqliteClassName(c state.SQLiteClass) string {
	switch c {
	case state.SQLiteOperationalError:
		return "OperationalError"
	case state.SQLiteIntegrityError:
		return "IntegrityError"
	case state.SQLiteInterfaceError:
		return "InterfaceError"
	case state.SQLiteInternalError:
		return "InternalError"
	case state.SQLiteDataError:
		return "DataError"
	case state.SQLiteProgrammingError:
		return "ProgrammingError"
	case state.SQLiteNotSupportedError:
		return "NotSupportedError"
	}
	return "DatabaseError"
}

// Reports whether the header write version byte marks a WAL database, reading without any lock
func usesWAL(src string) (bool, error) {
	f, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer f.Close()
	header := make([]byte, 19)
	n, err := io.ReadFull(f, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n == 19 && header[18] == 2, nil
}

// Measured copy speed of the stepped attempts
type copyRate struct {
	seconds float64
	pages   int
	total   int
}

// Pages copied per second of copy time, floored against a zero duration
func (r *copyRate) pagesPerSecond() float64 {
	return float64(r.pages) / math.Max(r.seconds, 1e-6)
}

// One stepped backup, false when a foreign write restarted it from page one
func steppedAttempt(src, dst pysqlite.RawConn, pages int, checkBusy func(status, remaining, total int) error, rate *copyRate) (bool, error) {
	lastRemaining := -1
	lastTick, lastBusy := monotonic(), false
	progress := func(status, remaining, total int) error {
		if err := checkBusy(status, remaining, total); err != nil {
			return err
		}
		now := monotonic()
		busySleep := 0.0
		if lastBusy {
			busySleep = rollbackBusySleep.Seconds()
		}
		stepSeconds := math.Max(0, now.Sub(lastTick).Seconds()-busySleep)
		lastTick, lastBusy = now, status == sqlite3.SQLITE_BUSY || status == sqlite3.SQLITE_LOCKED
		if status != sqlite3.SQLITE_OK && status != sqlite3.SQLITE_DONE {
			return nil
		}
		restarted := lastRemaining >= 0 && remaining >= lastRemaining
		rate.seconds += stepSeconds
		if lastRemaining < 0 || restarted {
			rate.pages += total - remaining
		} else {
			rate.pages += lastRemaining - remaining
		}
		rate.total = total
		if restarted {
			return errBackupRestarted
		}
		lastRemaining = remaining
		return nil
	}
	if err := connBackup(src, dst, pages, progress, rollbackBusySleep); err != nil {
		if errors.Is(err, errBackupRestarted) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Stepped copy of a rollback journal database, releasing the read lock between steps and refusing a long hold
func steppedRollbackJournalCopy(src, dst pysqlite.RawConn, checkBusy func(status, remaining, total int) error) error {
	rate := &copyRate{}
	pages := rollbackFirstStepPages
	for range rollbackMaxRestarts + 1 {
		done, err := steppedAttempt(src, dst, pages, checkBusy, rate)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		pages = max(rollbackFirstStepPages, int(rate.pagesPerSecond()*rollbackStepSeconds))
	}
	expectedHold := float64(rate.total) / math.Max(rate.pagesPerSecond(), 1.0)
	if expectedHold > rollbackLockHoldSeconds {
		return &runtimeError{"state.db uses a rollback journal and is written continuously; a consistent snapshot " +
			fmt.Sprintf("would block those writes for about %.1fs, so it was not taken. ", expectedHold) +
			"Retry when no agent turn is running."}
	}
	return connBackup(src, dst, -1, checkBusy, rollbackBusySleep)
}

// One long backup call on its own goroutine so a stop request ends the wait without waiting for the copy
type offThreadCopy struct {
	copy     func() error
	done     chan struct{}
	started  bool
	err      error
	panicked any
}

func newOffThreadCopy(copy func() error) *offThreadCopy {
	return &offThreadCopy{copy: copy, done: make(chan struct{})}
}

// Runs the copy, keeping its error or panic for the waiting caller
func (c *offThreadCopy) runCopy() {
	defer close(c.done)
	defer func() { c.panicked = recover() }()
	c.err = c.copy()
}

// Starts the copy and returns its outcome, or Interrupted as soon as the context stops
func (c *offThreadCopy) run(ctx context.Context) error {
	c.started = true
	go c.runCopy()
	select {
	case <-c.done:
	case <-ctx.Done():
		return &Interrupted{context.Cause(ctx)}
	}
	if c.panicked != nil {
		panic(c.panicked)
	}
	return c.err
}

// Reports whether the copy goroutine is still inside the backup call
func (c *offThreadCopy) running() bool {
	if !c.started {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Consistent WAL safe snapshot through the backup API, false and no destination when none can be made
func safeCopyDB(ctx context.Context, src, dst string, timeoutSeconds float64) (bool, error) {
	var conn, backupConn *sqliteConn
	var copier *offThreadCopy
	defer func() {
		if copier == nil || !copier.running() {
			closeQuietly(backupConn)
			closeQuietly(conn)
			return
		}
		src, dst := conn, backupConn
		go func() {
			<-copier.done
			closeQuietly(dst)
			closeQuietly(src)
		}()
	}()
	err := func() error {
		if err := secureCreate(dst); err != nil {
			return err
		}
		resolved, err := pypath.Resolve(src)
		if err != nil {
			return err
		}
		uri, err := pypath.AsURI(resolved)
		if err != nil {
			return err
		}
		if conn, err = openSQLite(ctx, uri+"?mode=ro&_pragma=busy_timeout(0)"); err != nil {
			return err
		}
		if backupConn, err = connectPath(ctx, dst); err != nil {
			return err
		}
		window := time.Duration(math.Max(0, timeoutSeconds) * float64(time.Second))
		busyDeadline := monotonic().Add(window)
		checkProgress := func(status, _, _ int) error {
			if ctx.Err() != nil {
				return &Interrupted{context.Cause(ctx)}
			}
			now := monotonic()
			if status == sqlite3.SQLITE_BUSY || status == sqlite3.SQLITE_LOCKED {
				if !now.Before(busyDeadline) {
					text, _ := pyrepr.Format(timeoutSeconds, "g")
					return &sqliteBackupTimeout{"database remained locked for " + text + " seconds"}
				}
			} else {
				busyDeadline = now.Add(window)
			}
			return nil
		}
		wal, err := usesWAL(src)
		if err != nil {
			return err
		}
		if !wal {
			return steppedRollbackJournalCopy(conn, backupConn, checkProgress)
		}
		src, dst := conn, backupConn
		copier = newOffThreadCopy(func() error { return connBackup(src, dst, -1, checkProgress, 100*time.Millisecond) })
		return copier.run(ctx)
	}()
	if err == nil {
		return true, nil
	}
	var stop *Interrupted
	if errors.As(err, &stop) {
		return false, stop
	}
	slog.Warn(fmt.Sprintf("SQLite safe copy failed for %s: %s", pypath.PathStr(src), excText(err)))
	closeQuietly(backupConn)
	backupConn = nil
	removeFile(dst)
	return false, nil
}

// Removes staging copies of a prefix older than any live pre-flight could be
func reclaimAbandonedStaging(h, prefix string) {
	horizon := time.Now().Add(-abandonedStagingSeconds * time.Second)
	staged, err := pypath.Glob(h, prefix+"*.partial*")
	if err != nil {
		return
	}
	for _, p := range staged {
		if info, err := os.Stat(p); err == nil && info.ModTime().Before(horizon) {
			os.Remove(p)
		}
	}
}

// Removes a path, a missing one counting as removed
func unlinkMissingOK(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Rows of a quick_check as Python prints the list of one string tuples
func quickCheckRepr(rows []string) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = "(" + pyrepr.StrRepr(r) + ",)"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Runs PRAGMA quick_check on a database file and returns its rows
func quickCheck(ctx context.Context, path string) ([]string, error) {
	c, err := connectPath(ctx, path)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(c)
	rows, err := c.c.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Timestamp of the emergency snapshot names, UTC with microseconds
func emergencyStamp(t time.Time) string {
	t = t.UTC()
	return t.Format("2006-01-02T15-04-05-") + fmt.Sprintf("%06d", t.Nanosecond()/1000) + "Z"
}

// Publishes a verified emergency snapshot of state.db, keeping the previous ones when it fails
func PreflightStateDB(ctx context.Context, h string) (*pyjson.Object, error) {
	result := pyjson.NewObject()
	source := filepath.Join(h, "state.db")
	if _, err := os.Stat(source); err != nil {
		result.Set("path", nil)
		result.Set("message", "state.db not found (fresh install?)")
		return result, nil
	}
	reclaimAbandonedStaging(h, emergencyPrefix)
	base := fmt.Sprintf("%s%s-%d", emergencyPrefix, emergencyStamp(time.Now()), os.Getpid())
	destination := filepath.Join(h, base+".bak")
	staged := filepath.Join(h, base+".partial")
	var size int64
	err := func() error {
		copied, err := safeCopyDB(ctx, source, staged, 10.0)
		if err != nil {
			return err
		}
		if !copied {
			return &runtimeError{"SQLite safe copy failed; previous emergency snapshots were retained"}
		}
		if ctx.Err() != nil {
			return &Interrupted{context.Cause(ctx)}
		}
		rows, err := quickCheck(ctx, staged)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0] != "ok" {
			return &runtimeError{"SQLite snapshot integrity check failed: " + quickCheckRepr(rows)}
		}
		info, err := os.Stat(staged)
		if err != nil {
			return err
		}
		size = info.Size()
		return os.Rename(staged, destination)
	}()
	if cleanErr := unlinkMissingOK(staged); cleanErr != nil {
		err = cleanErr
	} else if cleanErr := unlinkMissingOK(staged + "-journal"); cleanErr != nil {
		err = cleanErr
	}
	if err != nil {
		return nil, err
	}
	snapshots, err := pypath.Glob(h, emergencyPrefix+"*.bak")
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(snapshots)))
	for i, old := range snapshots {
		if i < 2 {
			continue
		}
		if ctx.Err() != nil {
			return nil, &Interrupted{context.Cause(ctx)}
		}
		if err := os.Remove(old); err != nil {
			slog.Warn(fmt.Sprintf("Could not prune emergency snapshot %s: %s", old, excText(err)))
		}
	}
	result.Set("path", destination)
	result.Set("bytes", size)
	return result, nil
}

var errSIGTERM = errors.New("SIGTERM")

// Pre-flight entry the Desktop runs with the home as its only argument, exiting 143 through cleanup on SIGTERM
func PreflightStateDBMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	defer signal.Stop(terms)
	go func() {
		select {
		case <-terms:
			cancel(errSIGTERM)
		case <-ctx.Done():
		}
	}()
	if len(args) < 1 {
		fmt.Fprintln(stderr, "IndexError: list index out of range")
		return 1
	}
	result, err := PreflightStateDB(ctx, args[0])
	if errors.Is(context.Cause(ctx), errSIGTERM) {
		return 128 + int(syscall.SIGTERM)
	}
	if err != nil {
		fmt.Fprintln(stderr, excLine(err))
		return 1
	}
	text, err := pyjson.Dumps(result)
	if err != nil {
		fmt.Fprintln(stderr, excLine(err))
		return 1
	}
	fmt.Fprintln(stdout, text)
	return 0
}
