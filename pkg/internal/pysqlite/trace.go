package pysqlite

import (
	"sync"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

var traceHandlers sync.Map

// Hands the expanded text of each statement that starts running to the trace function of its connection
func traceTrampoline(tls *libc.TLS, mask uint32, db, stmt, text uintptr) int32 {
	if mask != sqlite3.SQLITE_TRACE_STMT {
		return 0
	}
	h, ok := traceHandlers.Load(db)
	if !ok {
		return 0
	}
	sqlText := libc.GoString(text)
	if expanded := sqlite3.Xsqlite3_expanded_sql(tls, stmt); expanded != 0 {
		sqlText = libc.GoString(expanded)
		sqlite3.Xsqlite3_free(tls, expanded)
	}
	h.(func(string))(sqlText)
	return 0
}

// Installs a statement trace on the connection like sqlite3 Connection.set_trace_callback, nil removing it
func SetTraceCallback(conn RawConn, fn func(sql string)) error {
	return conn.Raw(func(driverConn any) error {
		db, tls, err := driverHandles(driverConn)
		if err != nil {
			return err
		}
		if fn == nil {
			traceHandlers.Delete(db)
			if rc := sqlite3.Xsqlite3_trace_v2(tls, db, 0, 0, 0); rc != sqlite3.SQLITE_OK {
				return targetError(tls, db)
			}
			return nil
		}
		traceHandlers.Store(db, fn)
		if rc := sqlite3.Xsqlite3_trace_v2(tls, db, uint32(sqlite3.SQLITE_TRACE_STMT), cFuncPointer(traceTrampoline), db); rc != sqlite3.SQLITE_OK {
			traceHandlers.Delete(db)
			return targetError(tls, db)
		}
		return nil
	})
}
