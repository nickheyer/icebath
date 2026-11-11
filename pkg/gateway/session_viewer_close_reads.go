package gateway

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nickheyer/icebath/pkg/state"
)

const viewerBusySQL = "SELECT 1 FROM session_admissions WHERE target_session_id=? AND status IN ('queued','started','unknown') " +
	"UNION ALL SELECT 1 FROM worker_executions WHERE session_id IN (?,?) " +
	"AND status IN ('registered','running','unknown') LIMIT 1"

// Physical transcript a viewer close acts on, the local receipt's entry or else the compression tip
func viewerPhysicalTarget(ctx context.Context, a *SessionAuthority, sessionID string) (string, error) {
	target, err := receiptEntryTarget(ctx, a.DB, sessionID)
	if err == nil {
		return target, nil
	}
	var refusal *state.RuntimeStoreError
	if class := checkErrorClass(err); !errors.As(err, &refusal) && class != "KeyError" && class != "TypeError" {
		return "", err
	}
	tip, err := a.DB.GetCompressionTip(ctx, sessionID)
	if err != nil || tip != "" {
		return tip, err
	}
	return sessionID, nil
}

// True while the session holds unsettled input or its transcripts hold a live worker execution
func viewerBusy(ctx context.Context, a *SessionAuthority, sessionID, target string) (bool, error) {
	busy := false
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var one int
		err := conn.QueryRowContext(ctx, viewerBusySQL, sessionID, sessionID, target).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			busy = false
			return nil
		}
		busy = err == nil
		return err
	})
	return busy, err
}
