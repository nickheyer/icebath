package state

import (
	"errors"
	"fmt"
	"reflect"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Connection exposing database/sql raw driver access
type rawConner interface {
	Raw(f func(driverConn any) error) error
}

var errNoSQLiteHandle = errors.New("connection exposes no SQLite handle")

// Runs fn on the SQLite handle and libc thread state behind conn while database/sql holds it exclusively
func withRawSQLite(conn SQLiteConn, fn func(tls *libc.TLS, db uintptr) error) error {
	for {
		switch c := conn.(type) {
		case *TrackedConn:
			conn = c.SQLiteConn
			continue
		case *lazyTxConn:
			conn = c.SQLiteConn
			continue
		case rawConner:
			return c.Raw(func(driverConn any) error {
				tls, db, err := sqliteHandleOf(driverConn)
				if err != nil {
					return err
				}
				return fn(tls, db)
			})
		}
		return fmt.Errorf("%w: %T", errNoSQLiteHandle, conn)
	}
}

// Handle and thread state the modernc driver keeps in its unexported connection fields
func sqliteHandleOf(driverConn any) (*libc.TLS, uintptr, error) {
	v := reflect.ValueOf(driverConn)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return nil, 0, fmt.Errorf("%w: %T", errNoSQLiteHandle, driverConn)
	}
	s := v.Elem()
	dbField, tlsField := s.FieldByName("db"), s.FieldByName("tls")
	if !dbField.IsValid() || dbField.Kind() != reflect.Uintptr || !tlsField.IsValid() ||
		tlsField.Type() != reflect.TypeFor[*libc.TLS]() {
		return nil, 0, fmt.Errorf("%w: %T", errNoSQLiteHandle, driverConn)
	}
	tls := *(**libc.TLS)(unsafe.Pointer(tlsField.UnsafeAddr()))
	if tls == nil || dbField.Uint() == 0 {
		return nil, 0, fmt.Errorf("%w: %T is closed", errNoSQLiteHandle, driverConn)
	}
	return tls, uintptr(dbField.Uint()), nil
}

// Reports whether no transaction is open on the connection, sqlite3_get_autocommit
func sqliteAutocommit(conn SQLiteConn) (bool, error) {
	auto := false
	err := withRawSQLite(conn, func(tls *libc.TLS, db uintptr) error {
		auto = sqlite3.Xsqlite3_get_autocommit(tls, db) != 0
		return nil
	})
	return auto, err
}

// Rows changed by every statement since the connection opened, the sqlite3 total_changes attribute
func sqliteTotalChanges(conn SQLiteConn) (int64, error) {
	var n int64
	err := withRawSQLite(conn, func(tls *libc.TLS, db uintptr) error {
		n = sqlite3.Xsqlite3_total_changes64(tls, db)
		return nil
	})
	return n, err
}
