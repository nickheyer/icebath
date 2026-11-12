package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Second handle on the session store of a case, the gateway side that writes the ledger
func ledgerHandle(t *testing.T, dbPath string) *state.DB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Admits one queued input for a session under a fresh gateway epoch, claiming it when asked
func admitLedgerInput(t *testing.T, db *state.DB, sessionID, text string, claimed bool) {
	t.Helper()
	ctx := context.Background()
	epoch, err := db.BeginRuntimeEpoch(ctx, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	payload := pyjson.NewObject()
	payload.Set("text", text)
	if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: "owner", SessionID: sessionID, RequestID: "r1", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if claimed {
		if _, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: sessionID}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReapLeavesRowOpenWhileTheLedgerHoldsItsTurn(t *testing.T) {
	for _, source := range []string{"gui", "tui"} {
		for _, claimed := range []bool{true, false} {
			name := source + "-queued"
			if claimed {
				name = source + "-started"
			}
			t.Run(name, func(t *testing.T) {
				sid := source + "-live"
				dbPath, session, a := realOwnedSession(t, source, sid)
				admitLedgerInput(t, ledgerHandle(t, dbPath), sid, "long turn", claimed)
				teardownSession(context.Background(), session, "ws_orphan_reap")
				endedAt, endReason := readRealRow(t, dbPath, sid)
				if endedAt != nil || endReason != nil || a.EndSessionOnClose() {
					t.Fatalf("ended_at %v end_reason %v end_on_close %v", endedAt, endReason, a.EndSessionOnClose())
				}
			})
		}
	}
}

func TestReapStillEndsAnIdleRow(t *testing.T) {
	dbPath, session, _ := realOwnedSession(t, "tui", "tui-idle")
	ledger := ledgerHandle(t, dbPath)
	if err := ledger.CreateSession(state.NewSession{ID: "other", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	admitLedgerInput(t, ledger, "other", "another session", false)
	teardownSession(context.Background(), session, "ws_orphan_reap")
	if _, endReason := readRealRow(t, dbPath, "tui-idle"); endReason != "ws_orphan_reap" {
		t.Fatalf("end_reason %v", endReason)
	}
}
