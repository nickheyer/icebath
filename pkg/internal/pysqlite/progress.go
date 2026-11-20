package pysqlite

import (
	"errors"
	"reflect"
	"sync"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Connection exposing its driver connection, satisfied by *sql.Conn and types embedding it
type RawConn interface {
	Raw(func(driverConn any) error) error
}

var progressHandlers sync.Map

// Calls the handler registered for the database handle, a nonzero result aborts the running statement
func progressTrampoline(_ *libc.TLS, db uintptr) int32 {
	h, ok := progressHandlers.Load(db)
	if !ok {
		return 0
	}
	if h.(func() int)() != 0 {
		return 1
	}
	return 0
}

// C function pointer of a Go function laid out as modernc callbacks expect
func cFuncPointer[T any](f T) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct{ f T }{f}))
}

// Database handle and thread state of a modernc.org/sqlite driver connection
func driverHandles(driverConn any) (uintptr, *libc.TLS, error) {
	v := reflect.ValueOf(driverConn)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct || v.Elem().Type().PkgPath() != "modernc.org/sqlite" {
		return 0, nil, errors.New("a modernc.org/sqlite driver connection is required")
	}
	db, tls := v.Elem().FieldByName("db"), v.Elem().FieldByName("tls")
	if !db.IsValid() || db.Kind() != reflect.Uintptr || !tls.IsValid() || tls.Type() != reflect.TypeFor[*libc.TLS]() {
		return 0, nil, errors.New("modernc.org/sqlite connection layout has no db and tls handles")
	}
	return uintptr(db.Uint()), (*libc.TLS)(unsafe.Pointer(tls.Pointer())), nil
}

// Runs handler every nOps virtual machine instructions like sqlite3 set_progress_handler, nil removes it
func SetProgressHandler(conn RawConn, handler func() int, nOps int) error {
	return conn.Raw(func(driverConn any) error {
		db, tls, err := driverHandles(driverConn)
		if err != nil {
			return err
		}
		if handler == nil || nOps <= 0 {
			progressHandlers.Delete(db)
			sqlite3.Xsqlite3_progress_handler(tls, db, 0, 0, 0)
			return nil
		}
		progressHandlers.Store(db, handler)
		sqlite3.Xsqlite3_progress_handler(tls, db, int32(nOps), cFuncPointer(progressTrampoline), db)
		return nil
	})
}
