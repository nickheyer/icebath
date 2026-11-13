package state

import (
	"errors"
	"regexp"
	"strings"
	"syscall"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Persistence failure bucket every storage notice keys its guidance on
type Cause string

// Buckets a persistence failure falls in
const (
	CauseLocked            Cause = "locked"
	CauseCompression       Cause = "compression"
	CauseCompressionClosed Cause = "compression_closed"
	CauseTurnLease         Cause = "turn_lease"
	CauseCorrupt           Cause = "corrupt"
	CauseFTSIndex          Cause = "fts_index"
	CauseReplaced          Cause = "replaced"
	CauseDeletedWAL        Cause = "deleted_wal"
	CauseDisk              Cause = "disk"
	CauseSessionRowMissing Cause = "session_row_missing"
	CauseUnknown           Cause = "unknown"
)

// Every bucket in the order consumers enumerate them
func Causes() []Cause {
	return []Cause{CauseLocked, CauseCompression, CauseCompressionClosed, CauseTurnLease, CauseCorrupt, CauseFTSIndex,
		CauseReplaced, CauseDeletedWAL, CauseDisk, CauseSessionRowMissing, CauseUnknown}
}

// Error that names its own bucket, consulted before any code or text
type causer interface {
	PersistenceCause() Cause
}

// Bucket of a store whose path names another file or whose WAL generation was lost
func (r *sentinel) PersistenceCause() Cause {
	if r == ErrDeletedWAL {
		return CauseDeletedWAL
	}
	return CauseReplaced
}

// Corruption bucket of a quarantined store
func (e *CorruptError) PersistenceCause() Cause { return CauseCorrupt }

// Text substrings of a structurally damaged database file, checked before the disk bucket
var corruptionMarkers = []string{"malformed", "file is not a database", "not a database", "database corruption"}

// Text substrings of SQLite reporting a malformed schema
var malformedSchemaMarkers = []string{"malformed database schema"}

// Text substrings of a malformed schema or a corrupt image
var malformedMarkers = []string{"malformed database schema", "database disk image is malformed"}

// Text substring of SQLITE_IOERR
const diskIOErrorMarker = "disk i/o error"

// Busy rather than gone, the HTTP 503 class, corruption deliberately absent
var transientMarkers = []string{diskIOErrorMarker, "database is locked", "database table is locked", "busy"}

// Text substrings of a filesystem that cannot take another write
var diskFullMarkers = []string{"no space left on device", "not enough space", "database or disk is full", "disk full", "full disk", "enospc"}

// Object names every full-text table and shadow table of the store carries
var ftsObject = regexp.MustCompile(`\bmessages_fts\w*`)

// Phrase table of buckets for text that lost its error type, in precedence order
var causePhrases = []struct {
	markers []string
	cause   Cause
}{
	{[]string{"turn lease"}, CauseTurnLease},
	{[]string{"closed by compression"}, CauseCompressionClosed},
	{[]string{"being compressed", "compression lease"}, CauseCompression},
	{[]string{"deleted state.db-wal", "deleted state.db-shm"}, CauseDeletedWAL},
	{[]string{"was replaced underneath"}, CauseReplaced},
	{corruptionMarkers, CauseCorrupt},
	{[]string{"locked", "busy"}, CauseLocked},
	{[]string{"foreign key constraint failed"}, CauseSessionRowMissing},
}

// SQLite result code err carries anywhere in its chain, the extended form when SQLite reported one
func resultCode(err error) (int, bool) {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code(), true
	}
	return 0, false
}

// Primary result code err carries, the low byte of an extended code
func primaryCode(err error) (int, bool) {
	code, ok := resultCode(err)
	return code & 0xff, ok
}

// Reports whether a primary code reports a failed call or unusable store rather than damaged content
func callCode(code int) bool {
	switch code & 0xff {
	case sqlite3.SQLITE_ERROR, sqlite3.SQLITE_PERM, sqlite3.SQLITE_ABORT, sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED,
		sqlite3.SQLITE_READONLY, sqlite3.SQLITE_INTERRUPT, sqlite3.SQLITE_IOERR, sqlite3.SQLITE_FULL,
		sqlite3.SQLITE_CANTOPEN, sqlite3.SQLITE_PROTOCOL, sqlite3.SQLITE_EMPTY, sqlite3.SQLITE_SCHEMA:
		return true
	}
	return false
}

// Which SQLite error class an error belongs to
type family int

// Not from SQLite, a failed call or unusable store, or content SQLite refused or found damaged
const (
	notSQLite family = iota
	callFamily
	contentFamily
)

// Family of err, the outermost engine error deciding before a driver error deeper in the chain
func familyOf(err error) family {
	var own interface{ callFailure() bool }
	if errors.As(err, &own) {
		if own.callFailure() {
			return callFamily
		}
		return contentFamily
	}
	if code, ok := resultCode(err); ok {
		if callCode(code) {
			return callFamily
		}
		return contentFamily
	}
	return notSQLite
}

// Lower cased message of err with every driver decoration stripped from SQLite errors
func lowerText(err error) string { return strings.ToLower(errText(err)) }

// Message of err as SQLite words it, the driver category prefix and code suffixes removed
func errText(err error) string {
	var se *sqlite.Error
	if !errors.As(err, &se) || err != error(se) {
		return err.Error()
	}
	return sqliteMessage(se)
}

// Statement message of a driver error without its category prefix and result code suffixes
func sqliteMessage(se *sqlite.Error) string {
	text := se.Error()
	if i := strings.LastIndex(text, " (SQLITE_"); i > 0 && strings.HasSuffix(text, ")") {
		text = text[:i]
	}
	if i := strings.LastIndex(text, " ("); i > 0 && strings.HasSuffix(text, ")") && digitsOnly(text[i+2:len(text)-1]) {
		text = text[:i]
	}
	if category := errorCategory(se.Code()); category != "" {
		text = strings.TrimPrefix(text, category+": ")
	}
	return text
}

// Category text SQLite gives a result code
func errorCategory(code int) string {
	tls := libc.NewTLS()
	defer tls.Close()
	return libc.GoString(sqlite3.Xsqlite3_errstr(tls, int32(code)))
}

// Reports whether s is one or more ASCII digits
func digitsOnly(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// Reports whether text holds any marker
func containsAny(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// SQLITE_BUSY or SQLITE_LOCKED by result code, the text deciding only for errors without one
func isLockError(err error) bool {
	if code, ok := primaryCode(err); ok {
		return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
	}
	text := lowerText(err)
	return strings.Contains(text, "locked") || strings.Contains(text, "busy")
}

// Transient engine error on contended WAL appends that retries like lock contention
func isNoMoreRows(err error) bool { return strings.Contains(lowerText(err), "no more rows available") }

// Disk I/O error by text, matching wrapped and driver errors alike
func isDiskIOError(err error) bool { return strings.Contains(lowerText(err), diskIOErrorMarker) }

// Reports a malformed schema or a corrupt image, for diagnostics and offline recovery only
func IsMalformed(err error) bool {
	return err != nil && familyOf(err) != notSQLite && containsAny(lowerText(err), malformedMarkers)
}

// Reports SQLite's explicit malformed schema text, the one corruption runtime repair may act on
func IsMalformedSchema(err error) bool {
	return err != nil && familyOf(err) != notSQLite && containsAny(lowerText(err), malformedSchemaMarkers)
}

// Reports busy right now rather than damaged, the split between retry or 503 and a hard failure
func IsTransient(err error) bool {
	return err != nil && familyOf(err) == callFamily && (isLockError(err) || containsAny(lowerText(err), transientMarkers))
}

// Reports a disk full condition from ENOSPC, SQLITE_FULL or matching text
func IsDiskFull(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	return containsAny(lowerText(err), diskFullMarkers)
}

// Reports corruption SQLite attributes to the full-text index layer, a result code outranking text
func IsFTSScopedCorruption(err error) bool {
	if err == nil {
		return false
	}
	if code, ok := resultCode(err); ok {
		return code == sqlite3.SQLITE_CORRUPT_VTAB
	}
	return ftsScopedText(lowerText(err))
}

// Reports text that names a full-text object together with a corruption report
func ftsScopedText(text string) bool {
	if !ftsObject.MatchString(text) {
		return false
	}
	if strings.HasPrefix(text, "fts5:") && strings.Contains(text, "corrupt") {
		return true
	}
	return containsAny(text, corruptionMarkers)
}

// Bucket of a persistence failure, its type first, then result codes, then text that survived wrapping
func Classify(err error) Cause {
	if err == nil {
		return CauseUnknown
	}
	var c causer
	if errors.As(err, &c) {
		return c.PersistenceCause()
	}
	if IsFTSScopedCorruption(err) {
		return CauseFTSIndex
	}
	if code, ok := primaryCode(err); ok && (code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED) {
		return CauseLocked
	}
	if code, ok := resultCode(err); ok && code == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
		return CauseSessionRowMissing
	}
	text := lowerText(err)
	for _, p := range causePhrases {
		if containsAny(text, p.markers) {
			return p.cause
		}
	}
	if IsDiskFull(err) || containsAny(text, []string{"disk", "readonly", "read-only"}) {
		return CauseDisk
	}
	return CauseUnknown
}

// Canonical B-tree, schema or freelist damage outside the full-text index and the repairable malformed schema
func isStructuralCorruption(err error) bool {
	return err != nil && familyOf(err) != notSQLite && !IsFTSScopedCorruption(err) && !IsMalformedSchema(err) &&
		Classify(err) == CauseCorrupt && corruptByCode(err)
}

// Reports SQLITE_CORRUPT or SQLITE_NOTADB for an error carrying a result code, true for one that lost its code
func corruptByCode(err error) bool {
	code, ok := primaryCode(err)
	return !ok || code == sqlite3.SQLITE_CORRUPT || code == sqlite3.SQLITE_NOTADB
}

// Driver error shown with the message SQLite gave the statement, its result code still reachable underneath
type driverError struct {
	err *sqlite.Error
}

// SQLite statement message without driver decoration
func (d *driverError) Error() string { return sqliteMessage(d.err) }

// Driver error carrying the result code
func (d *driverError) Unwrap() error { return d.err }

// Error as the engine hands it to callers, a bare driver error reworded to SQLite's own message
func presentable(err error) error {
	var se *sqlite.Error
	if err != nil && errors.As(err, &se) && err == error(se) {
		return &driverError{err: se}
	}
	return err
}
