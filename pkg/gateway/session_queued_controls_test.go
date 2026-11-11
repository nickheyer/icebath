package gateway

import (
	"context"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Records the session as launched from the given surface, an interactive local one by default
func launchedFrom(t *testing.T, ctx context.Context, db *state.SessionDB, source string) {
	t.Helper()
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET source=? WHERE id=?", source, "s")
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyInteractiveLocalSessionsAreOfferedQueuedControls(t *testing.T) {
	ctx, db, a := newTestAuthority(t, "")
	row := pyjson.ObjectOf("status", "queued", "target_session_id", "s", "payload", pyjson.ObjectOf("text", "hello"))
	controls, err := queuedControls(ctx, a, row)
	if err != nil || len(controls) != 0 {
		t.Fatalf("no launch policy: %v %v", controls, err)
	}
	for _, c := range []struct {
		source  string
		offered bool
	}{{"gui", true}, {"desktop", true}, {"tui", true}, {"cli", true}, {"acp", true}, {"cron", false}, {"kanban", false}, {"a2a", false}, {"bot_room", false}} {
		a.launchSources = map[string]any{}
		launchedFrom(t, ctx, db, c.source)
		controls, err := queuedControls(ctx, a, row)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{}
		if c.offered {
			want = []string{"steer", "replace"}
		}
		if !slices.Equal(controls, want) {
			t.Fatalf("%s: controls %v, want %v", c.source, controls, want)
		}
	}
}

func TestPendingRowsNameTheControlsTheOwnerServes(t *testing.T) {
	ctx, db, a := newTestAuthority(t, "")
	launchedFrom(t, ctx, db, "gui")
	holdDrains(t)
	for _, requestID := range []string{"head", "follower"} {
		if _, err := submitText(ctx, a, requestID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	rows, err := a.pendingRowsMasked(ctx, testRef)
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]*pyjson.Object{}
	for _, row := range rows {
		receipt, err := a.pendingReceipt(ctx, row)
		if err != nil {
			t.Fatal(err)
		}
		dict := pendingAdmissionDict(receipt)
		pending[rowString(dict, "input_id")] = dict
	}
	if controls, _ := pending["follower"].Get("controls"); !pyjson.Equal(controls, []any{"steer", "replace"}) {
		t.Fatalf("follower controls %v", controls)
	}
	if controls, _ := pending["head"].Get("controls"); pyTruthy(controls) {
		t.Fatalf("a started row has no queued row controls, got %v", controls)
	}
}

func TestTheSteerTransactionFencesOnTheQueuedRowAndTheRunningGeneration(t *testing.T) {
	ctx, db, a := newTestAuthority(t, "")
	launchedFrom(t, ctx, db, "gui")
	for _, requestID := range []string{"running", "follow-up"} {
		if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: a.Epoch, PrincipalID: "human", SessionID: "s",
			RequestID: requestID, Payload: pyjson.ObjectOf("text", requestID)}); err != nil {
			t.Fatal(err)
		}
	}
	running, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: a.Epoch, SessionID: "s"})
	if err != nil || running == nil {
		t.Fatalf("claim %v %v", running, err)
	}
	follow, err := db.FindSessionAdmission(ctx, "human", "s", "follow-up")
	if err != nil || follow == nil {
		t.Fatalf("follow-up %v %v", follow, err)
	}
	generation := *rowInt(running, "generation")
	if _, _, err := db.SteerSessionInput(ctx, a.Epoch, rowString(follow, "admission_id"), generation+1); refusalReason(err) != "session_idle" {
		t.Fatalf("stale generation: %v", err)
	}
	if _, _, err := db.SteerSessionInput(ctx, a.Epoch, rowString(running, "admission_id"), generation); refusalReason(err) != "admission_started" {
		t.Fatalf("own claim: %v", err)
	}
	row, fresh, err := db.SteerSessionInput(ctx, a.Epoch, rowString(follow, "admission_id"), generation)
	if err != nil {
		t.Fatal(err)
	}
	outcome, _ := row.Get("outcome")
	if rowString(row, "status") != "terminal" || outcome != "steering" || *rowInt(row, "generation") != generation || !fresh {
		t.Fatalf("transfer row %v fresh %v", row, fresh)
	}
	replay, fresh, err := db.SteerSessionInput(ctx, a.Epoch, rowString(follow, "admission_id"), generation)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, _ := replay.Get("outcome"); outcome != "steering" || fresh {
		t.Fatalf("replay %v fresh %v", replay, fresh)
	}
}
