package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Recovers committed API work once the real API adapter is published, each served profile under its own scope
func RecoverAPITurns(ctx context.Context, adapter PlatformAdapter) error {
	for _, authority := range servingAuthorities(adapter.Base().GatewayRunner) {
		if err := recoverAuthorityAPITurns(ownerScope(ctx, authority, false), authority); err != nil {
			return err
		}
	}
	return nil
}

// Collects unheld images, compacts settled payloads and reschedules every queued API session that may still run
func recoverAuthorityAPITurns(ctx context.Context, a *SessionAuthority) error {
	if _, err := collectUnheldAPIImages(ctx, a.DB); err != nil {
		return err
	}
	if _, err := compactSettledAPIPayloads(ctx, a.DB, nil); err != nil {
		return err
	}
	var targets []string
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		rows, err := state.QueryObjects(ctx, conn,
			"SELECT DISTINCT target_session_id FROM session_admissions WHERE principal_id='api' AND status='queued'")
		for _, row := range rows {
			targets = append(targets, pyrepr.Str(row.GetOr("target_session_id", nil)))
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, sid := range targets {
		err := recoverAPISession(ctx, a, sid)
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			slog.Warn(fmt.Sprintf("API session %s paused: %s", sid, refusal.Reason))
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Restores one queued API session and schedules its drain unless a lost turn pauses it or a turn is refused
func recoverAPISession(ctx context.Context, a *SessionAuthority, sid string) error {
	if err := restoreAPISession(ctx, a, sid); err != nil {
		return err
	}
	ref := SessionRef{a.ProfileID, sid}
	pending, err := a.DB.ListSessionAdmissions(ctx, sid, true)
	if err != nil {
		return err
	}
	for _, row := range pending {
		if rowString(row, "status") == AdmissionUnknown {
			return nil
		}
	}
	for _, row := range pending {
		if rowString(row, "status") != AdmissionQueued {
			continue
		}
		raw, err := pyItem(row, "payload")
		if err != nil {
			return err
		}
		payload, isDict := raw.(*pyjson.Object)
		if !isDict {
			return &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(raw) + "' object has no attribute 'get'"}
		}
		if _, err := checkAPITurn(ctx, a, ref, payload); err != nil {
			return err
		}
	}
	return scheduleSession(ctx, a, ref)
}
