//go:build !windows

package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// First row of a read query as a column mapping
func readRow(t *testing.T, ctx context.Context, db *state.SessionDB, query string, args ...any) map[string]any {
	t.Helper()
	var row map[string]any
	if err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var err error
		row, err = state.ConnRow(ctx, conn, query, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

// Owned cli session, its runtime epoch and a human principal admission claimed as started
func claimedHumanTurn(t *testing.T, requestID string) (context.Context, *state.SessionDB, *SessionAuthority, *pyjson.Object) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "owned", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, t.TempDir(), "owner", db, epoch)
	if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "owned",
		RequestID: requestID, Payload: pyjson.NewObject()}); err != nil {
		t.Fatal(err)
	}
	row, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db, a, row
}

func TestWorkerLossFencesSameGenerationParentResult(t *testing.T) {
	ctx, db, a, row := claimedHumanTurn(t, "input")
	child := stdinReaderChild(t)
	scope, err := reserveAdmissionWorker(ctx, a, rowString(row, "admission_id"), child, "human", childHello(t, child), "")
	if err != nil {
		t.Fatal(err)
	}
	_ = child.cmd.Process.Kill()
	child.wait(5)
	if err := loseAdmissionWorker(ctx, a, row, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := retainResult(ctx, db, a.Epoch, row, pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "STALE"), "usage", pyjson.NewObject())); !isRuntimeRefusal(err, "stale_generation") {
		t.Fatalf("retain after loss: %v", err)
	}
	if status := readRow(t, ctx, db, "SELECT status FROM session_admissions")["status"]; status != "unknown" {
		t.Fatalf("admission status %v", status)
	}
	if status := readRow(t, ctx, db, "SELECT status FROM worker_executions")["status"]; status != "terminal" {
		t.Fatalf("worker status %v", status)
	}
}

func TestLateBootstrapHandshakeCannotRearmARecoveredAdmission(t *testing.T) {
	ctx, db, a, row := claimedHumanTurn(t, "lost")
	lost := rowString(row, "admission_id")
	follower, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: a.Epoch, PrincipalID: "human", SessionID: "owned",
		RequestID: "next", Payload: pyjson.NewObject()})
	if err != nil {
		t.Fatal(err)
	}
	child := stdinReaderChild(t)
	scope, err := reserveAdmissionWorker(ctx, a, lost, child, "human", childHello(t, child), "")
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := db.BeginRuntimeEpoch(ctx, "restarted")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecoverSessionInputs(ctx, restarted); err != nil {
		t.Fatal(err)
	}
	owner := NewSessionAuthority(&authorityRunnerDouble{}, a.ProfileID, "restarted", db, restarted)
	connection := &AuthorityConnection{authority: owner, actor: Principal{Subject: "human", ProfileID: a.ProfileID,
		Capabilities: map[string]bool{"worker:adopt": true}}}
	if _, err := workerRequest(ctx, connection, SessionRef{}, scopeIdentity(scope), "adopt"); !isRuntimeRefusal(err, "stale_epoch") {
		t.Fatalf("late adopt: %v", err)
	}
	admission := readRow(t, ctx, db, "SELECT status,owner_epoch FROM session_admissions WHERE admission_id=?", lost)
	if admission["status"] != "unknown" || admission["owner_epoch"] != a.Epoch {
		t.Fatalf("admission %v", admission)
	}
	worker := readRow(t, ctx, db, "SELECT status,owner_epoch FROM worker_executions")
	if worker["status"] != "unknown" || worker["owner_epoch"] != a.Epoch {
		t.Fatalf("worker %v", worker)
	}
	if _, err := db.ResolveUnknownSessionInput(ctx, restarted, lost, rowGeneration(row), nil, nil); err != nil {
		t.Fatal(err)
	}
	next, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: restarted, SessionID: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	if rowString(next, "admission_id") != rowString(follower, "admission_id") {
		t.Fatalf("claimed %v want %v", rowString(next, "admission_id"), rowString(follower, "admission_id"))
	}
}
