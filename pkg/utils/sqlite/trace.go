package sqliteutil

import (
	"strings"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

var traces registry[func(sql string)]

// Hands each statement that starts running to the trace of its database handle with its parameters written in, also a statement run from inside another one's step, whose SQL SQLite reports behind --, and the -- TRIGGER comment of a trigger the statement fires as is; a panic in the trace logged and dropped
func traceTrampoline(tls *libc.TLS, mask uint32, db, stmt, text uintptr) int32 {
	if mask != sqlite3.SQLITE_TRACE_STMT {
		return 0
	}
	fn, ok := traces.get(db)
	if !ok {
		return 0
	}
	sql := libc.GoString(text)
	own := sqlite3.Xsqlite3_sql(tls, stmt)
	if text == own || own != 0 && strings.TrimPrefix(sql, "-- ") == libc.GoString(own) {
		if expanded := sqlite3.Xsqlite3_expanded_sql(tls, stmt); expanded != 0 {
			sql = libc.GoString(expanded)
			sqlite3.Xsqlite3_free(tls, expanded)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			logPanic("trace", r)
		}
	}()
	fn(sql)
	return 0
}

// Installs fn as the statement trace of conn: it receives each statement as it starts running with its bound parameters written in, statements run from inside another one's step included, and the -- TRIGGER comment SQLite gives for each trigger that fires; nil removes it
func SetTrace(conn RawConn, fn func(sql string)) error {
	return withHandle(conn, func(h handle) error {
		if fn == nil {
			rc := sqlite3.Xsqlite3_trace_v2(h.tls, h.db, 0, 0, 0)
			traces.remove(h)
			if rc != sqlite3.SQLITE_OK {
				return codeError(h.tls, h.db, rc)
			}
			return nil
		}
		traces.set(h, fn)
		if rc := sqlite3.Xsqlite3_trace_v2(h.tls, h.db, sqlite3.SQLITE_TRACE_STMT, cFuncPointer(traceTrampoline), h.db); rc != sqlite3.SQLITE_OK {
			traces.remove(h)
			return codeError(h.tls, h.db, rc)
		}
		return nil
	})
}
