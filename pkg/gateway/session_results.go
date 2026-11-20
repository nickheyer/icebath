package gateway

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nickheyer/icebath/pkg/state"
)

// Saved terminal result of an admission, nil while it is not terminal or nothing was saved
func admissionResult(ctx context.Context, db *state.SessionDB, admissionID string) (any, error) {
	var out any
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		row, err := state.RuntimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		if rowString(row, "status") != AdmissionTerminal {
			return nil
		}
		var saved string
		err = conn.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key=?", state.TerminalResultPrefix+admissionID).Scan(&saved)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out, err = state.LoadTerminalResult(saved)
		return err
	})
	return out, err
}
