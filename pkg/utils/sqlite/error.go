package sqliteutil

import (
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Failure SQLite reported on a connection, its extended result code and the database's message; the Code method matches the one modernc.org/sqlite errors have
type Error struct {
	code int
	msg  string
}

// The database's message, as in database is locked
func (e *Error) Error() string { return e.msg }

// Extended result code, its low eight bits the primary code such as sqlite3.SQLITE_BUSY
func (e *Error) Code() int { return e.code }

// Whether a result code, extended or primary, reports an operational failure: one of the database's state or surroundings such as a missing table or column, a lock, a read-only or full disk, an interrupt or a changed schema, not of the statement's values or the caller's use of the API
func IsOperational(code int) bool {
	switch code & 0xff {
	case sqlite3.SQLITE_ERROR, sqlite3.SQLITE_PERM, sqlite3.SQLITE_ABORT, sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED,
		sqlite3.SQLITE_READONLY, sqlite3.SQLITE_INTERRUPT, sqlite3.SQLITE_IOERR, sqlite3.SQLITE_FULL,
		sqlite3.SQLITE_CANTOPEN, sqlite3.SQLITE_PROTOCOL, sqlite3.SQLITE_EMPTY, sqlite3.SQLITE_SCHEMA:
		return true
	}
	return false
}

// Error the database handle holds after a failed call
func handleError(tls *libc.TLS, db uintptr) error {
	return &Error{
		code: int(sqlite3.Xsqlite3_extended_errcode(tls, db)),
		msg:  libc.GoString(sqlite3.Xsqlite3_errmsg(tls, db)),
	}
}

// Error of a call that returned rc: the handle's own error when it records that code, else the generic text of rc, since some failures such as API misuse leave the handle's error untouched
func codeError(tls *libc.TLS, db uintptr, rc int32) error {
	if code := sqlite3.Xsqlite3_extended_errcode(tls, db); code&0xff == rc&0xff {
		return &Error{code: int(code), msg: libc.GoString(sqlite3.Xsqlite3_errmsg(tls, db))}
	}
	return &Error{code: int(rc), msg: libc.GoString(sqlite3.Xsqlite3_errstr(tls, rc))}
}
