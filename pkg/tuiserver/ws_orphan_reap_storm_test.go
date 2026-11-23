package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Launch store of the case home holding a tui row for each key
func reapLedger(t *testing.T, keys ...string) *state.DB {
	t.Helper()
	db := reaperTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	useSharedDB(t, db)
	for _, key := range keys {
		if err := db.CreateSession(state.NewSession{ID: key, Source: "tui"}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// End reason the store recorded for a session, empty while it is open
func storedEndReason(t *testing.T, db *state.DB, sid string) string {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	return strOr(row.GetOr("end_reason", nil))
}

// Whether a reap timer still counts down, false once it was cancelled
func reapArmed(timer *wsReapTimer) bool { return timer.timer.Stop() }

// Plain write peer of a resumed client, the id keeps two peers distinct
type liveWritePeer struct{ id int }

func (*liveWritePeer) Write(*pyjson.Object) bool { return true }
func (*liveWritePeer) Close()                    {}

// Agent double whose close holds the teardown until released, the slow teardown of the reap
type slowClosingAgent struct{ started, release chan struct{} }

func (a *slowClosingAgent) CloseContext(context.Context) error {
	close(a.started)
	<-a.release
	return nil
}

// Delegation field whose truth test raises, a registry lookup that fails
type raisingTruth struct{}

func (raisingTruth) PyBool() bool { panic("delegation registry unavailable") }
