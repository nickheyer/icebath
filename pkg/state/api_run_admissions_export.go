package state

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Every row of one statement on a connection as ordered objects
func QueryObjects(ctx context.Context, conn SQLiteConn, query string, args ...any) ([]*jsonutil.Object, error) {
	return queryObjects(ctx, conn, query, args...)
}

// Public admission dict of a raw ledger row, its payload decoded and its digest and lineage dropped
func AdmissionRow(row *jsonutil.Object) (*jsonutil.Object, error) { return admissionRow(row) }

// Exact retry of an admission on a retired target, nil for a live one, refused for a retired miss or conflict
func (d *SessionDB) RetryTerminalAdmission(ctx context.Context, in SessionInput) (*jsonutil.Object, error) {
	return d.retryTerminalAdmission(ctx, in)
}
