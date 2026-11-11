package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Seconds a settled exact retry keeps its byte exact reply
const RetryHorizonS = retryHorizonS

// First row of one statement on a connection as an ordered object, nil when it returns none
func QueryRowObject(ctx context.Context, conn SQLiteConn, query string, args ...any) (*pyjson.Object, error) {
	rows, err := queryObjects(ctx, conn, query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// Resolves a title collision inside an open write, the ValueError of a title another session holds
func (d *SessionDB) ResolveTitleConflictOnConn(ctx context.Context, conn SQLiteConn, sessionID, title string) (string, bool, error) {
	return d.resolveTitleConflict(ctx, conn, sessionID, title)
}

// Message fields that only carry compaction scaffolding
func CompactionInternalFields() []string { return append([]string(nil), compactionInternalFields...) }
