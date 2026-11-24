package sqliteutil

import (
	"math"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

var progressHandlers registry[func() bool]

// Asks the handler of the database handle whether to stop, a panic counting as a request to stop
func progressTrampoline(_ *libc.TLS, db uintptr) (stop int32) {
	fn, ok := progressHandlers.get(db)
	if !ok {
		return 0
	}
	defer func() {
		if r := recover(); r != nil {
			logPanic("progress handler", r)
			stop = 1
		}
	}()
	if fn() {
		return 1
	}
	return 0
}

// Calls interrupt about every n virtual machine steps of each statement running on conn, the statement failing as interrupted once it answers true; a nil interrupt or an n that is not positive removes the handler
func SetProgressHandler(conn RawConn, interrupt func() bool, n int) error {
	return withHandle(conn, func(h handle) error {
		if interrupt == nil || n <= 0 {
			sqlite3.Xsqlite3_progress_handler(h.tls, h.db, 0, 0, 0)
			progressHandlers.remove(h)
			return nil
		}
		progressHandlers.set(h, interrupt)
		sqlite3.Xsqlite3_progress_handler(h.tls, h.db, int32(min(n, math.MaxInt32)), cFuncPointer(progressTrampoline), h.db)
		return nil
	})
}
