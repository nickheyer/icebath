package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Every row of one statement on a connection as ordered objects
func QueryObjects(ctx context.Context, conn SQLiteConn, query string, args ...any) ([]*pyjson.Object, error) {
	return queryObjects(ctx, conn, query, args...)
}

// Public admission dict of a raw ledger row, its payload decoded and its digest and lineage dropped
func AdmissionRow(row *pyjson.Object) (*pyjson.Object, error) { return admissionRow(row) }

// Exact retry of an admission on a retired target, nil for a live one, refused for a retired miss or conflict
func (d *SessionDB) RetryTerminalAdmission(ctx context.Context, in SessionInput) (*pyjson.Object, error) {
	return d.retryTerminalAdmission(ctx, in)
}
