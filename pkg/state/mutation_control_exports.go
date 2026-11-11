package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Owner identity of a session row, None for a Desktop account label
func OwnerUser(row *pyjson.Object) (any, error) { return ownerUser(row) }

// Normalized import sessions and the problems that refuse them
func (d *SessionDB) ValidateImportPayload(sessions []any) ([]*pyjson.Object, []any, error) {
	return d.validateImportPayload(sessions)
}

// Creation id owning the local lineage a row belongs to, read on the given connection
func LocalLineageOwner(ctx context.Context, conn SQLiteConn, sessionID string) (string, error) {
	return localLineageOwner(ctx, conn, sessionID)
}

// First row of a query on the connection as an object, nil when there is none
func ConnObject(ctx context.Context, conn SQLiteConn, query string, args ...any) (*pyjson.Object, error) {
	return connObject(ctx, conn, query, args...)
}
