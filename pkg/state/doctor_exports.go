package state

import "context"

// Reports a path on a virtiofs or 9p filesystem shared across a VM boundary
func PathOnCrossVMFS(path string) (bool, error) { return pathOnCrossVMFS(path) }

// Repair hint naming what an update can do about the WAL reset bug for this install type
func WALResetRepairHint(ctx context.Context) string { return walResetRepairHint(ctx) }

// Probes a database on a fresh connection, returning the reason it is unhealthy or false when it opens cleanly
func DBOpensCleanly(ctx context.Context, dbPath string) (string, bool, error) {
	return dbOpensCleanly(ctx, dbPath)
}

// Whether a foreign holder or a positive exclusive lock probe says the database lacks a proven quiet state
func LiveWriterHoldsStateDB(ctx context.Context, dbPath string) (bool, error) {
	return liveWriterHoldsDB(ctx, dbPath)
}

// Runs fn with one connection excluding writers, or with the error that kept exclusion away
func ExclusiveRepairDBGuard(ctx context.Context, dbPath string, fn func(guard SQLiteConn, guardErr error) error) error {
	return exclusiveRepairDBGuard(ctx, dbPath, fn)
}

// Copies every page of one open database into another through the online backup interface
func BackupSQLite(src, dst SQLiteConn) error { return sqliteBackup(src, dst, -1, nil, 0.25) }
