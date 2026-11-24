package sqliteutil

import (
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Decision on one access SQLite asks about while it prepares a statement: the action code, its two detail arguments (table and column for reads and updates), the database name and the innermost trigger or view, absent arguments empty; it answers AuthOK, AuthDeny or AuthIgnore
type Authorizer func(action int, arg1, arg2, database, trigger string) int

const (
	// Verdict letting the access go ahead
	AuthOK = sqlite3.SQLITE_OK
	// Verdict failing the statement as not authorized
	AuthDeny = sqlite3.SQLITE_DENY
	// Verdict letting the statement run with that access silently skipped, a column read giving NULL
	AuthIgnore = sqlite3.SQLITE_IGNORE
	// Action of reading a column, arguments table and column; the other action codes are the SQLITE_ constants of modernc.org/sqlite/lib
	ActionRead = sqlite3.SQLITE_READ
	// Action of inserting into a table, first argument the table
	ActionInsert = sqlite3.SQLITE_INSERT
	// Action of updating a column, arguments table and column
	ActionUpdate = sqlite3.SQLITE_UPDATE
	// Action of deleting from a table, first argument the table
	ActionDelete = sqlite3.SQLITE_DELETE
)

var authorizers registry[Authorizer]

// Hands one access request to the authorizer of its database handle, a panic denying the access
func authorizerTrampoline(_ *libc.TLS, db uintptr, action int32, a1, a2, a3, a4 uintptr) (verdict int32) {
	fn, ok := authorizers.get(db)
	if !ok {
		return sqlite3.SQLITE_OK
	}
	defer func() {
		if r := recover(); r != nil {
			logPanic("authorizer", r)
			verdict = sqlite3.SQLITE_DENY
		}
	}()
	return int32(fn(int(action), libc.GoString(a1), libc.GoString(a2), libc.GoString(a3), libc.GoString(a4)))
}

// Installs auth as the authorizer of conn, consulted for every access of each statement prepared from then on; nil removes it
func SetAuthorizer(conn RawConn, auth Authorizer) error {
	return withHandle(conn, func(h handle) error {
		if auth == nil {
			rc := sqlite3.Xsqlite3_set_authorizer(h.tls, h.db, 0, 0)
			authorizers.remove(h)
			if rc != sqlite3.SQLITE_OK {
				return codeError(h.tls, h.db, rc)
			}
			return nil
		}
		authorizers.set(h, auth)
		if rc := sqlite3.Xsqlite3_set_authorizer(h.tls, h.db, cFuncPointer(authorizerTrampoline), h.db); rc != sqlite3.SQLITE_OK {
			authorizers.remove(h)
			return codeError(h.tls, h.db, rc)
		}
		return nil
	})
}
