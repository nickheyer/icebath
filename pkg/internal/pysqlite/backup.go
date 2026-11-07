package pysqlite

import (
	"errors"
	"time"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// sqlite3 exception a connection reports, carrying the result code and the database message
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Python sqlite3 exception class the result code raises
func (e *Error) PyTypeName() string {
	switch e.Code & 0xff {
	case sqlite3.SQLITE_INTERNAL, sqlite3.SQLITE_NOTFOUND:
		return "InternalError"
	case sqlite3.SQLITE_NOMEM:
		return "MemoryError"
	case sqlite3.SQLITE_ERROR, sqlite3.SQLITE_PERM, sqlite3.SQLITE_ABORT, sqlite3.SQLITE_BUSY,
		sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_READONLY, sqlite3.SQLITE_INTERRUPT, sqlite3.SQLITE_IOERR,
		sqlite3.SQLITE_FULL, sqlite3.SQLITE_CANTOPEN, sqlite3.SQLITE_PROTOCOL, sqlite3.SQLITE_EMPTY,
		sqlite3.SQLITE_SCHEMA:
		return "OperationalError"
	case sqlite3.SQLITE_TOOBIG:
		return "DataError"
	case sqlite3.SQLITE_CONSTRAINT, sqlite3.SQLITE_MISMATCH:
		return "IntegrityError"
	case sqlite3.SQLITE_MISUSE, sqlite3.SQLITE_RANGE:
		return "InterfaceError"
	}
	return "DatabaseError"
}

// Copies the main database of src into dst like sqlite3 Connection.backup, progress errors aborting the copy
func Backup(src, dst RawConn, pages int, progress func(status, remaining, total int) error, sleep time.Duration) error {
	if pages == 0 {
		pages = -1
	}
	if src == dst {
		return errors.New("target cannot be the same connection instance")
	}
	return dst.Raw(func(dstDriver any) error {
		dstDB, tls, err := driverHandles(dstDriver)
		if err != nil {
			return err
		}
		return src.Raw(func(srcDriver any) error {
			srcDB, _, err := driverHandles(srcDriver)
			if err != nil {
				return err
			}
			if sqlite3.Xsqlite3_get_autocommit(tls, dstDB) == 0 {
				return &Error{Code: sqlite3.SQLITE_ERROR, Msg: "target is in transaction"}
			}
			return backupLoop(tls, srcDB, dstDB, pages, progress, sleep)
		})
	})
}

// Steps one backup handle to completion, sleeping after busy steps and raising the target error on finish
func backupLoop(tls *libc.TLS, srcDB, dstDB uintptr, pages int, progress func(status, remaining, total int) error, sleep time.Duration) error {
	name, err := libc.CString("main")
	if err != nil {
		return err
	}
	defer libc.Xfree(tls, name)
	handle := sqlite3.Xsqlite3_backup_init(tls, dstDB, name, srcDB, name)
	if handle == 0 {
		return targetError(tls, dstDB)
	}
	for {
		rc := sqlite3.Xsqlite3_backup_step(tls, handle, int32(pages))
		if progress != nil {
			remaining := int(sqlite3.Xsqlite3_backup_remaining(tls, handle))
			total := int(sqlite3.Xsqlite3_backup_pagecount(tls, handle))
			if err := progress(int(rc), remaining, total); err != nil {
				sqlite3.Xsqlite3_backup_finish(tls, handle)
				return err
			}
		}
		if rc == sqlite3.SQLITE_BUSY || rc == sqlite3.SQLITE_LOCKED {
			sqlite3.Xsqlite3_sleep(tls, int32(sleep/time.Millisecond))
		}
		if rc != sqlite3.SQLITE_OK && rc != sqlite3.SQLITE_BUSY && rc != sqlite3.SQLITE_LOCKED {
			break
		}
	}
	if sqlite3.Xsqlite3_backup_finish(tls, handle) != sqlite3.SQLITE_OK {
		return targetError(tls, dstDB)
	}
	return nil
}

// Error the target connection holds after a failed backup call
func targetError(tls *libc.TLS, db uintptr) error {
	return &Error{Code: int(sqlite3.Xsqlite3_errcode(tls, db)), Msg: libc.GoString(sqlite3.Xsqlite3_errmsg(tls, db))}
}
