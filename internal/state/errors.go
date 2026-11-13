package state

import (
	"errors"
	"fmt"
	"time"
)

// Guide every storage refusal links
const RecoveryDocsURL = "https://icebath.app/docs/user-guide/session-storage-recovery"

// Refusal text of a store whose path now names another file
const replacedMsg = "FATAL: state.db was replaced underneath the gateway; refusing further " +
	"writes to this file. Divert transcripts to sessions/<id>.jsonl (and the " +
	"gateway pending_messages spool) and restore or reopen after operator intervention."

// Refusal text of a store whose WAL generation another process still holds deleted
const deletedWALMsg = "FATAL: session storage stopped writing because another icebath process still holds a deleted " +
	"state.db-wal or state.db-shm inode (an old copy of the write-ahead log). Nothing is lost: quit " +
	"every icebath process on this profile (Desktop app, gateway, dashboard, cron), run `icebath doctor` " +
	"(it names the processes still holding the log), then start icebath again. Do not delete the WAL " +
	"yourself and do not run `icebath doctor --fix` while they are running. " +
	"Guide: " + RecoveryDocsURL + " " +
	"Detail: the path names a different (or missing) generation than the one this process holds " +
	"open; opening or writing through it would mint a second WAL (split-brain). " +
	"database.journal_mode: delete is operator containment, not a new default."

// Refusal text of a store quarantined after structural corruption
const corruptMsg = "FATAL: state.db reported structural corruption (database disk image is " +
	"malformed outside the FTS shadow tables) on a live handle; refusing further " +
	"writes, automatic reopen, and the close-time WAL checkpoint on this file. " +
	"Stop the gateway, then run `icebath sessions recover --source <state.db> " +
	"--inspect-only` or restore a snapshot. Unwritten transcripts are diverted to " +
	"sessions/<id>.jsonl (and the gateway pending_messages spool)."

// Meta key holding the once-per-file generation stamp
const generationKey = "db_file_generation"

// Fixed text a store refuses with, possibly counting as a broader refusal too
type sentinel struct {
	msg    string
	parent error
}

// Refusal text
func (r *sentinel) Error() string { return r.msg }

// Broader refusal this one also counts as
func (r *sentinel) Unwrap() error { return r.parent }

// Refusals every transcript diversion owner handles the same way
var (
	// Path no longer names the file the store opened, so writes stop
	ErrReplaced error = &sentinel{msg: replacedMsg}
	// A live process holds a deleted WAL or SHM generation, matching ErrReplaced too
	ErrDeletedWAL error = &sentinel{msg: deletedWALMsg, parent: ErrReplaced}
)

// Error a store returns once it was closed and may not reopen
var ErrClosed = errors.New("state database is closed")

// Error a write callback wraps when a live compression lock refuses the write for a few seconds
var ErrCompressionInProgress = errors.New("session is being compressed")

// Quarantine of a store after structural corruption outside the full-text index
type CorruptError struct {
	prefix string
	Reason string
	cause  error
}

// Prefix, the quarantine text and the first cause
func (e *CorruptError) Error() string {
	return e.prefix + corruptMsg + " (cause: " + e.Reason + ")"
}

// Corruption error that tripped the quarantine
func (e *CorruptError) Unwrap() error { return e.cause }

// Corruption class of the SQLite error family
func (e *CorruptError) callFailure() bool { return false }

// Write lock another process kept past the whole patience budget
type LockedError struct {
	Patience time.Duration
	cause    error
}

// Busy refusal that names maintenance as the likely holder and the store as healthy
func (e *LockedError) Error() string {
	return fmt.Sprintf("database is locked (another icebath process held the state.db write lock for over %.0fs — "+
		"likely a long maintenance operation such as VACUUM, a large WAL checkpoint, or an older pre-update "+
		"process; the database itself is healthy)", e.Patience.Seconds())
}

// Last busy error SQLite reported
func (e *LockedError) Unwrap() error { return e.cause }

// Lock contention is a failed call, never damaged content
func (e *LockedError) callFailure() bool { return true }

// Retired WAL generation the store could not capture durably, the handle staying open for a retry
type CaptureError struct {
	msg   string
	cause error
}

// What failed in the capture
func (e *CaptureError) Error() string { return e.msg }

// Filesystem error behind the failure
func (e *CaptureError) Unwrap() error { return e.cause }

// Error the engine raises in the SQLite family without a result code
type dbError struct {
	msg   string
	call  bool
	cause error
}

// Engine message
func (e *dbError) Error() string { return e.msg }

// Error the message was raised from
func (e *dbError) Unwrap() error { return e.cause }

// Whether the error reports a failed call rather than damaged content
func (e *dbError) callFailure() bool { return e.call }

// Engine error reporting a failed call or unusable store
func callErr(cause error, format string, args ...any) error {
	return &dbError{msg: fmt.Sprintf(format, args...), call: true, cause: cause}
}

// Engine error reporting content SQLite cannot read
func contentErr(cause error, format string, args ...any) error {
	return &dbError{msg: fmt.Sprintf(format, args...), cause: cause}
}
