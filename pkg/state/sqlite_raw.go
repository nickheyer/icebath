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

// Error of the last failed call on a SQLite handle with its extended code and message
func sqliteHandleError(tls *libc.TLS, db uintptr) error {
	code := int(sqlite3.Xsqlite3_extended_errcode(tls, db))
	return &SQLiteError{Class: classForCode(code), Msg: libc.GoString(sqlite3.Xsqlite3_errmsg(tls, db)), Code: code, HasCode: true}
}

// Copies the main database of src into dst through the online backup API, the sqlite3 Connection.backup loop
func sqliteBackup(src, dst SQLiteConn, pages int, progress func(status, remaining, total int) error, sleepSeconds float64) error {
	return withRawSQLite(src, func(_ *libc.TLS, srcDB uintptr) error {
		return withRawSQLite(dst, func(tls *libc.TLS, dstDB uintptr) error {
			if sqlite3.Xsqlite3_get_autocommit(tls, dstDB) == 0 {
				return &SQLiteError{Class: SQLiteOperationalError, Msg: "target is in transaction"}
			}
			zMain, err := libc.CString("main")
			if err != nil {
				return err
			}
			defer libc.Xfree(tls, zMain)
			bck := sqlite3.Xsqlite3_backup_init(tls, dstDB, zMain, srcDB, zMain)
			if bck == 0 {
				return sqliteHandleError(tls, dstDB)
			}
			var callbackErr error
			for {
				rc := sqlite3.Xsqlite3_backup_step(tls, bck, int32(pages))
				if progress != nil {
					remaining := int(sqlite3.Xsqlite3_backup_remaining(tls, bck))
					total := int(sqlite3.Xsqlite3_backup_pagecount(tls, bck))
					if callbackErr = progress(int(rc), remaining, total); callbackErr != nil {
						break
					}
				}
				if rc == sqlite3.SQLITE_BUSY || rc == sqlite3.SQLITE_LOCKED {
					sqlite3.Xsqlite3_sleep(tls, int32(sleepSeconds*1000))
				}
				if rc != sqlite3.SQLITE_OK && rc != sqlite3.SQLITE_BUSY && rc != sqlite3.SQLITE_LOCKED {
					break
				}
			}
			rc := sqlite3.Xsqlite3_backup_finish(tls, bck)
			if callbackErr != nil {
				return callbackErr
			}
			if rc != sqlite3.SQLITE_OK {
				return sqliteHandleError(tls, dstDB)
			}
			return nil
		})
	})
}
