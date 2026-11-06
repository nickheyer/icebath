package pysqlite

import (
	sqlite3 "modernc.org/sqlite/lib"
)

// Reports whether a transaction is open on the connection like sqlite3 Connection.in_transaction
func InTransaction(conn RawConn) (bool, error) {
	open := false
	err := conn.Raw(func(driverConn any) error {
		db, tls, err := driverHandles(driverConn)
		if err != nil {
			return err
		}
		open = sqlite3.Xsqlite3_get_autocommit(tls, db) == 0
		return nil
	})
	return open, err
}
