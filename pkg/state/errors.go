package state

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var malformedSchemaMarkers = []string{"malformed database schema"}

var malformedDBMarkers = append(append([]string{}, malformedSchemaMarkers...), "database disk image is malformed")

const diskIOErrorMarker = "disk i/o error"

var transientSQLiteMarkers = []string{diskIOErrorMarker, "database is locked", "database table is locked", "busy"}

var sqliteLockCodes = []int{sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED}

// Reports whether a primary result code is lock contention
func isLockCode(code int) bool {
	for _, lock := range sqliteLockCodes {
		if code == lock {
			return true
		}
	}
	return false
}

var diskFullMarkers = []string{"no space left on device", "not enough space", "database or disk is full", "disk full", "full disk", "enospc"}

// Lists every bucket ClassifyPersistenceError returns
var PersistenceErrorCauses = []string{
	"locked", "compression", "compression_closed", "turn_lease", "corrupt", "fts_index",
	"replaced", "deleted_wal", "disk", "session_row_missing", "unknown",
}

var dbCorruptionMarkers = []string{"malformed", "file is not a database", "not a database", "database corruption"}

// The extended result code SQLite gives corruption inside a virtual table
const SQLiteCorruptVtab = 267

var ftsObjectRE = regexp.MustCompile(`\bmessages_fts\w*`)

// Names the Python sqlite3 exception class an error belongs to
type SQLiteClass int

const (
	SQLiteDatabaseError SQLiteClass = iota + 1
	SQLiteOperationalError
	SQLiteIntegrityError
	SQLiteInterfaceError
	SQLiteInternalError
	SQLiteDataError
	SQLiteProgrammingError
	SQLiteNotSupportedError
)

// A sqlite3 exception raised by state code itself with a class, message and optional result code
type SQLiteError struct {
	Class   SQLiteClass
	Msg     string
	Code    int
	HasCode bool
}

func (e *SQLiteError) Error() string { return e.Msg }

// Class CPython's sqlite3 module raises for a result code
func classForCode(code int) SQLiteClass {
	switch code & 0xff {
	case sqlite3.SQLITE_INTERNAL, sqlite3.SQLITE_NOTFOUND:
		return SQLiteInternalError
	case sqlite3.SQLITE_ERROR, sqlite3.SQLITE_PERM, sqlite3.SQLITE_ABORT, sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED,
		sqlite3.SQLITE_READONLY, sqlite3.SQLITE_INTERRUPT, sqlite3.SQLITE_IOERR, sqlite3.SQLITE_FULL,
		sqlite3.SQLITE_CANTOPEN, sqlite3.SQLITE_PROTOCOL, sqlite3.SQLITE_EMPTY, sqlite3.SQLITE_SCHEMA:
		return SQLiteOperationalError
	case sqlite3.SQLITE_TOOBIG:
		return SQLiteDataError
	case sqlite3.SQLITE_CONSTRAINT, sqlite3.SQLITE_MISMATCH:
		return SQLiteIntegrityError
	case sqlite3.SQLITE_MISUSE, sqlite3.SQLITE_RANGE:
		return SQLiteInterfaceError
	}
	return SQLiteDatabaseError
}

// Python sqlite3 exception class of an error, false when it is no sqlite3 exception
func sqliteClassOf(err error) (SQLiteClass, bool) {
	if err == nil {
		return 0, false
	}
	var own *SQLiteError
	if errors.As(err, &own) {
		return own.Class, true
	}
	var corrupt *StateDBCorruptError
	if errors.As(err, &corrupt) {
		return SQLiteDatabaseError, true
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		return classForCode(se.Code()), true
	}
	return 0, false
}

// Python sqlite3 exception class of an error for callers outside this package, false when it is no sqlite3 exception
func SQLiteClassOf(err error) (SQLiteClass, bool) { return sqliteClassOf(err) }

// Reports whether an error is a sqlite3.DatabaseError or one of its subclasses
func isDatabaseError(err error) bool {
	class, ok := sqliteClassOf(err)
	return ok && class != SQLiteInterfaceError
}

// Reports whether an error is a sqlite3.OperationalError
func isOperationalError(err error) bool {
	class, ok := sqliteClassOf(err)
	return ok && class == SQLiteOperationalError
}

// Extended result code carried by an error, false when none is known
func sqliteErrorCode(v any) (int, bool) {
	err, ok := v.(error)
	if !ok || err == nil {
		return 0, false
	}
	var own *SQLiteError
	if errors.As(err, &own) {
		return own.Code, own.HasCode
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code(), true
	}
	return 0, false
}

// Primary result code carried by an error, extended codes keeping it in the low byte
func sqlitePrimaryCode(v any) (int, bool) {
	code, ok := sqliteErrorCode(v)
	return code & 0xff, ok
}

var (
	errstrMu  sync.Mutex
	errstrTLS *libc.TLS
)

// Canonical sqlite3_errstr text of a result code
func sqliteErrstr(code int) string {
	errstrMu.Lock()
	defer errstrMu.Unlock()
	if errstrTLS == nil {
		errstrTLS = libc.NewTLS()
	}
	return libc.GoString(sqlite3.Xsqlite3_errstr(errstrTLS, int32(code)))
}

// The sqlite3_errmsg text Python shows for a driver error, without the driver's errstr and code decorations
func sqliteErrmsg(se *sqlite.Error) string {
	msg := strings.TrimSuffix(se.Error(), " (SQLITE_BUSY)")
	msg = strings.TrimSuffix(msg, fmt.Sprintf(" (%d)", se.Code()))
	return strings.TrimPrefix(msg, sqliteErrstr(se.Code())+": ")
}

// Text Python's str() gives an exception or string, None for nil
func errText(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case error:
		text := x.Error()
		var se *sqlite.Error
		if errors.As(x, &se) {
			text = strings.Replace(text, se.Error(), sqliteErrmsg(se), 1)
		}
		return text
	}
	return fmt.Sprint(v)
}

// str() of an exception value with sqlite3 messages rendered as CPython prints them
func ErrText(v any) string { return errText(v) }

// Reports whether text contains any marker
func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// Reports a malformed schema or a generic corrupt image error, for diagnostics and offline recovery
func IsMalformedDBError(err error) bool {
	return isDatabaseError(err) && containsAny(strings.ToLower(errText(err)), malformedDBMarkers)
}

// Reports SQLITE_BUSY or SQLITE_LOCKED by result code, or by text when no code is known
func IsSQLiteLockError(v any) bool {
	if code, ok := sqlitePrimaryCode(v); ok {
		return isLockCode(code)
	}
	text := strings.ToLower(errText(v))
	return strings.Contains(text, "locked") || strings.Contains(text, "busy")
}

// Reports the transient no more rows engine error of contended WAL appends
func isNoMoreRows(err error) bool {
	return strings.Contains(strings.ToLower(errText(err)), "no more rows available")
}

// Reports a busy right now operational error that retries instead of failing
func IsTransientSQLiteError(err error) bool {
	return isOperationalError(err) && (IsSQLiteLockError(err) || containsAny(strings.ToLower(errText(err)), transientSQLiteMarkers))
}

// Reports only SQLite's explicit malformed schema text
func IsMalformedSchemaError(err error) bool {
	return isDatabaseError(err) && containsAny(strings.ToLower(errText(err)), malformedSchemaMarkers)
}

// Reports ENOSPC, SQLITE_FULL or text naming a full disk
func IsDiskFullError(v any) bool {
	if v == nil {
		return false
	}
	if err, ok := v.(error); ok {
		if err == nil {
			return false
		}
		if errors.Is(err, syscall.ENOSPC) {
			return true
		}
	}
	return containsAny(strings.ToLower(errText(v)), diskFullMarkers)
}

// Reports corruption SQLite attributes to the FTS index layer, a known code outranking text
func IsFTSScopedCorruptionError(v any) bool {
	if v == nil {
		return false
	}
	if err, ok := v.(error); ok && err == nil {
		return false
	}
	if code, ok := sqliteErrorCode(v); ok {
		return code == SQLiteCorruptVtab
	}
	text := strings.ToLower(errText(v))
	if !ftsObjectRE.MatchString(text) {
		return false
	}
	if strings.HasPrefix(text, "fts5:") && strings.Contains(text, "corrupt") {
		return true
	}
	return containsAny(text, dbCorruptionMarkers)
}

// Rejects a durable write to a parent compression already closed
type CompressionSessionClosedError struct {
	SessionID string
}

func (e *CompressionSessionClosedError) Error() string {
	return "Session " + pyRepr(e.SessionID) + " is closed by compression; adopt its live continuation before appending messages"
}

// Rejects a non owner write while compression owns the session
type CompressionSessionBusyError struct{ Msg string }

func (e *CompressionSessionBusyError) Error() string { return e.Msg }

// The transient busy case of a live compression lock
type SessionCompressionInProgressError struct{ Msg string }

func (e *SessionCompressionInProgressError) Error() string { return e.Msg }

// Lets the error match its CompressionSessionBusyError parent class
func (e *SessionCompressionInProgressError) As(target any) bool {
	if t, ok := target.(**CompressionSessionBusyError); ok {
		*t = &CompressionSessionBusyError{Msg: e.Msg}
		return true
	}
	return false
}

// Stops an in place prune that could not name the exact live row
type PruneRowUnresolvedError struct{ Msg string }

func (e *PruneRowUnresolvedError) Error() string { return e.Msg }

// Stops an in place prune whose named row another writer already rewrote
type PruneRowStaleError struct{ Msg string }

func (e *PruneRowStaleError) Error() string { return e.Msg }

// Lets the error match its PruneRowUnresolvedError parent class
func (e *PruneRowStaleError) As(target any) bool {
	if t, ok := target.(**PruneRowUnresolvedError); ok {
		*t = &PruneRowUnresolvedError{Msg: e.Msg}
		return true
	}
	return false
}

// Aborts a full writer commit whose held row was rewritten in place
type HeldRowRewrittenError struct{ Msg string }

func (e *HeldRowRewrittenError) Error() string { return e.Msg }

// Fences a transcript write whose turn lease holder lost the lease
type SessionTurnLeaseLostError struct{ Msg string }

func (e *SessionTurnLeaseLostError) Error() string { return e.Msg }

// Rejects a deletion an active turn lease or compression lock protects
type SessionActiveWriteGuardError struct{ Msg string }

func (e *SessionActiveWriteGuardError) Error() string { return e.Msg }

// Reports that the database path no longer names the file this handle opened
type StateDBReplacedError struct{ Msg string }

func (e *StateDBReplacedError) Error() string { return e.Msg }

// Reports a retired WAL generation that could not be captured durably, nothing having been mutated
type RetiredGenerationCaptureError struct{ Msg string }

func (e *RetiredGenerationCaptureError) Error() string { return e.Msg }

// Reports a live process holding a deleted wal or shm generation
type DeletedWalGenerationError struct{ Msg string }

func (e *DeletedWalGenerationError) Error() string { return e.Msg }

// Lets the error match its StateDBReplacedError parent class
func (e *DeletedWalGenerationError) As(target any) bool {
	if t, ok := target.(**StateDBReplacedError); ok {
		*t = &StateDBReplacedError{Msg: e.Msg}
		return true
	}
	return false
}

// Quarantines a handle that observed structural corruption, a sqlite3.DatabaseError subclass
type StateDBCorruptError struct {
	Msg   string
	Cause error
}

func (e *StateDBCorruptError) Error() string { return e.Msg }

// Cause the quarantine was raised from, carrying its SQLite result code
func (e *StateDBCorruptError) Unwrap() error { return e.Cause }

const stateDBApplicationIDOffset = 68

const stateDBGenerationKey = "db_file_generation"

// The message of a write refused on a replaced database file
const StateDBReplacedMsg = "FATAL: state.db was replaced underneath the gateway; refusing further " +
	"writes to this file. Divert transcripts to sessions/<id>.jsonl (and the " +
	"gateway pending_messages spool) and restore or reopen after operator intervention."

// The session storage recovery guide
const StorageRecoveryDocsURL = "https://hermes-agent.nousresearch.com/docs/user-guide/session-storage-recovery"

// The message of a write refused while a deleted wal generation is held
const DeletedWalGenerationMsg = "FATAL: session storage stopped writing because another icebath process still holds a deleted " +
	"state.db-wal or state.db-shm inode (an old copy of the write-ahead log). Nothing is lost: quit " +
	"every icebath process on this profile (Desktop app, gateway, dashboard, cron), run `icebath doctor` " +
	"(it names the processes still holding the log), then start icebath again. Do not delete the WAL " +
	"yourself and do not run `icebath doctor --fix` while they are running. " +
	"Guide: " + StorageRecoveryDocsURL + " " +
	"Detail: the path names a different (or missing) generation than the one this process holds " +
	"open; opening or writing through it would mint a second WAL (split-brain). " +
	"database.journal_mode: delete is operator containment, not a new default."

// The message of a handle quarantined after structural corruption
const StateDBCorruptMsg = "FATAL: state.db reported structural corruption (database disk image is " +
	"malformed outside the FTS shadow tables) on a live handle; refusing further " +
	"writes, automatic reopen, and the close-time WAL checkpoint on this file. " +
	"Stop the gateway, then run `icebath sessions recover --source <state.db> " +
	"--inspect-only` or restore a snapshot. Unwritten transcripts are diverted to " +
	"sessions/<id>.jsonl (and the gateway pending_messages spool)."

// Cause bucket of an error by its class, ordered so subclasses win over parents
func persistenceCauseByType(err error) (string, bool) {
	var leaseLost *SessionTurnLeaseLostError
	var closed *CompressionSessionClosedError
	var busy *CompressionSessionBusyError
	var deletedWal *DeletedWalGenerationError
	var replaced *StateDBReplacedError
	var corrupt *StateDBCorruptError
	switch {
	case errors.As(err, &leaseLost):
		return "turn_lease", true
	case errors.As(err, &closed):
		return "compression_closed", true
	case errors.As(err, &busy):
		return "compression", true
	case errors.As(err, &deletedWal):
		return "deleted_wal", true
	case errors.As(err, &replaced):
		return "replaced", true
	case errors.As(err, &corrupt):
		return "corrupt", true
	}
	return "", false
}

var persistenceCauseByPhrase = []struct {
	markers []string
	cause   string
}{
	{[]string{"turn lease"}, "turn_lease"},
	{[]string{"closed by compression"}, "compression_closed"},
	{[]string{"being compressed", "compression lease"}, "compression"},
	{[]string{"deleted state.db-wal", "deleted state.db-shm"}, "deleted_wal"},
	{[]string{"was replaced underneath"}, "replaced"},
	{dbCorruptionMarkers, "corrupt"},
	{[]string{"locked", "busy"}, "locked"},
	{[]string{"foreign key constraint failed"}, "session_row_missing"},
}

// Buckets an error or string into one of PersistenceErrorCauses
func ClassifyPersistenceError(v any) string {
	if v == nil {
		return "unknown"
	}
	if err, ok := v.(error); ok {
		if err == nil {
			return "unknown"
		}
		if cause, ok := persistenceCauseByType(err); ok {
			return cause
		}
	}
	if IsFTSScopedCorruptionError(v) {
		return "fts_index"
	}
	if code, ok := sqlitePrimaryCode(v); ok && isLockCode(code) {
		return "locked"
	}
	if code, ok := sqliteErrorCode(v); ok && code == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
		return "session_row_missing"
	}
	text := strings.ToLower(errText(v))
	for _, phrase := range persistenceCauseByPhrase {
		if containsAny(text, phrase.markers) {
			return phrase.cause
		}
	}
	if IsDiskFullError(v) || containsAny(text, []string{"disk", "readonly", "read-only"}) {
		return "disk"
	}
	return "unknown"
}
