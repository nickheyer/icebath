package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Admission rows of the target session in queue order, only queued started or unknown ones when pending only
func ListSessionAdmissions(ctx context.Context, db *SessionDB, sessionID string, pendingOnly bool) ([]*pyjson.Object, error) {
	query := "SELECT * FROM session_admissions WHERE target_session_id=? "
	if pendingOnly {
		query += "AND status IN ('queued','started','unknown') "
	}
	query += "ORDER BY seq"
	var out []*pyjson.Object
	err := db.ReadCtx(ctx, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, query, sessionID)
		if err != nil {
			return err
		}
		out = make([]*pyjson.Object, 0, len(rows))
		for _, raw := range rows {
			row, err := admissionRow(raw)
			if err != nil {
				return err
			}
			out = append(out, row)
		}
		return nil
	})
	return out, err
}
