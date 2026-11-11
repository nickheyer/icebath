package state

// Reports whether a transaction is open on the connection, the sqlite3 in_transaction attribute
func InTransaction(conn SQLiteConn) (bool, error) {
	auto, err := sqliteAutocommit(conn)
	return !auto, err
}
