package pysqlite

import (
	"sync"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Authorizer deciding one schema access from its action code and argument strings, NULL arguments arriving empty
type Authorizer func(action int, arg1, arg2, dbName, trigger string) int

// Authorizer verdicts and the action codes hermes tests inspect, named as sqlite3 exposes them
const (
	AuthOK       = sqlite3.SQLITE_OK
	AuthDeny     = sqlite3.SQLITE_DENY
	AuthIgnore   = sqlite3.SQLITE_IGNORE
	ActionRead   = sqlite3.SQLITE_READ
	ActionInsert = sqlite3.SQLITE_INSERT
	ActionUpdate = sqlite3.SQLITE_UPDATE
	ActionDelete = sqlite3.SQLITE_DELETE
	ActionSelect = sqlite3.SQLITE_SELECT
)

var authorizers sync.Map

// Routes one sqlite3 authorization request to the Go authorizer of its database handle
func authorizerTrampoline(_ *libc.TLS, db uintptr, action int32, a1, a2, a3, a4 uintptr) int32 {
	h, ok := authorizers.Load(db)
	if !ok {
		return sqlite3.SQLITE_OK
	}
	return int32(h.(Authorizer)(int(action), libc.GoString(a1), libc.GoString(a2), libc.GoString(a3), libc.GoString(a4)))
}

// Installs the authorizer of the connection like sqlite3 Connection.set_authorizer, nil removing it
func SetAuthorizer(conn RawConn, auth Authorizer) error {
	return conn.Raw(func(driverConn any) error {
		db, tls, err := driverHandles(driverConn)
		if err != nil {
			return err
		}
		if auth == nil {
			authorizers.Delete(db)
			if rc := sqlite3.Xsqlite3_set_authorizer(tls, db, 0, 0); rc != sqlite3.SQLITE_OK {
				return targetError(tls, db)
			}
			return nil
		}
		authorizers.Store(db, auth)
		if rc := sqlite3.Xsqlite3_set_authorizer(tls, db, cFuncPointer(authorizerTrampoline), db); rc != sqlite3.SQLITE_OK {
			authorizers.Delete(db)
			return targetError(tls, db)
		}
		return nil
	})
}
