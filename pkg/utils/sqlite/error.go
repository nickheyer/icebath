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
