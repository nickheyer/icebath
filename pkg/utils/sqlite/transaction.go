package sqliteutil

import sqlite3 "modernc.org/sqlite/lib"

// Whether a transaction is open on conn
func InTransaction(conn RawConn) (bool, error) {
	open := false
	err := withHandle(conn, func(h handle) error {
		open = sqlite3.Xsqlite3_get_autocommit(h.tls, h.db) == 0
		return nil
	})
	return open, err
}
