package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Transcript notice of an automation turn the owner settled interrupted
const automationInterruptedNotice = "An automated delivery was interrupted and was not run again. Some actions may already have run; verify their effects."

// True for an admission no person submitted, so nobody can be asked to resend it
func isUnattended(row *pyjson.Object) bool {
	principal := rowString(row, "principal_id")
	if principal == state.CronPrincipalID || strings.HasPrefix(principal, "automation:") {
		return true
	}
	payload := rowPayload(row)
	native := pyjson.NewObject()
	if value, _ := payload.Get("native_text_v1"); pyTruthy(value) {
		if obj, ok := value.(*pyjson.Object); ok {
			native = obj
		}
	}
	for _, key := range []string{"local_automation_v1", "kanban_card_v1"} {
		if _, ok := payload.Get(key); ok {
			return true
		}
	}
	for _, key := range []string{"automation", "webhook_delivery"} {
		if _, ok := native.Get(key); ok {
			return true
		}
	}
	return false
}

// Unknown rows no person submitted
func unattendedUnknown(rows []*pyjson.Object) []*pyjson.Object {
	var out []*pyjson.Object
	for _, row := range rows {
		if rowString(row, "status") == AdmissionUnknown && isUnattended(row) {
			out = append(out, row)
		}
	}
	return out
}

// Settles every unattended unknown turn at boot before any FIFO runs, returning how many it settled
func settleRecoveredUnattended(ctx context.Context, db *state.SessionDB, epoch int64) (int, error) {
	var rows []*pyjson.Object
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		rows = rows[:0]
		listed, err := conn.QueryContext(ctx, "SELECT admission_id FROM session_admissions WHERE status='unknown' ORDER BY seq")
		if err != nil {
			return err
		}
		var ids []string
		for listed.Next() {
			var id string
			if err := listed.Scan(&id); err != nil {
				listed.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := listed.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			row, err := state.RuntimeAdmission(ctx, conn, id)
			if err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	settled := 0
	for _, row := range unattendedUnknown(rows) {
		admissionID := rowString(row, "admission_id")
		var generation int64
		if stamped := rowInt(row, "generation"); stamped != nil {
			generation = *stamped
		}
		_, err := db.ResolveUnknownSessionInput(ctx, epoch, admissionID, generation,
			func(writeCtx context.Context, conn state.SQLiteConn, lost *pyjson.Object) error {
				_, err := closeDiscardedTurn(writeCtx, db, conn, lost, automationInterruptedNotice, nil, nil)
				return err
			}, nil)
		if err != nil {
			var refusal *state.RuntimeStoreError
			if errors.As(err, &refusal) {
				slog.Warn(fmt.Sprintf("Lost automation admission %s left unknown: %s", admissionID, refusal.Reason))
				continue
			}
			return settled, err
		}
		if err := releaseAdmissionMedia(ctx, db, admissionID); err != nil {
			return settled, err
		}
		settled++
		slog.Info(fmt.Sprintf("Lost automation admission %s on session %s settled interrupted (not replayed)",
			admissionID, rowString(row, "target_session_id")))
	}
	return settled, nil
}

// Settles the unattended unknown rows the drain found at its head, true when any settled
func settleUnattendedUnknown(ctx context.Context, a *SessionAuthority, ref SessionRef) (bool, error) {
	rows, err := a.DB.ListSessionAdmissions(ctx, ref.SessionID, true)
	if err != nil {
		return false, err
	}
	settled := false
	for _, row := range unattendedUnknown(rows) {
		var generation int64
		if stamped := rowInt(row, "generation"); stamped != nil {
			generation = *stamped
		}
		if _, err := a.ResolveUnknownAsOwner(ctx, ref, rowString(row, "admission_id"), generation, automationInterruptedNotice); err != nil {
			var refusal *state.RuntimeStoreError
			if errors.As(err, &refusal) {
				slog.Warn(fmt.Sprintf("Lost automation admission %s left unknown: %s", rowString(row, "admission_id"), refusal.Reason))
				continue
			}
			return settled, err
		}
		settled = true
	}
	return settled, nil
}
