package tuiserver

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/state"
)

// Creates a tui row and stamps its end a minute back, the stale close of the field shape
func stampEnded(t *testing.T, db *state.DB, rowID, reason string) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: rowID, Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(rowID, reason); err != nil {
		t.Fatal(err)
	}
	reaperExec(t, db, "UPDATE sessions SET ended_at = ? WHERE id = ?", float64(time.Now().UnixNano())/1e9-60.0, rowID)
}

// Runs one prompt through the real dispatch and waits until the worker settled
func submitAndSettle(t *testing.T, hz *turnHarness, text string) bool {
	t.Helper()
	hz.session.Set("running", true)
	started := runPromptSubmit(hz.ctx, "rid", hz.sid, hz.session, text, promptSubmitOptions{})
	for deadline := time.Now().Add(60 * time.Second); truthy(hz.session.Get("running")); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("turn never settled")
		}
	}
	return started
}

func TestARegisteredStaleCloseIsClearedBeforeTheTurnAndRotationPublishes(t *testing.T) {
	hz := newTurnHarness(t, []any{fakellm.Text{Text: "ok"}}, "")
	db := getDB(context.Background())
	stampEnded(t, db, hz.key, "tui_close")
	if !submitAndSettle(t, hz, "go") {
		t.Fatal("turn refused on a registered session")
	}
	requests := hz.srv.MainRequests()
	if len(requests) != 1 {
		t.Fatalf("turns %d", len(requests))
	}
	row, err := db.GetSession(context.Background(), hz.key)
	if err != nil || row.GetOr("end_reason", nil) != nil {
		t.Fatalf("stamp kept %v %v", row, err)
	}
	if !db.TryAcquireCompressionLock(hz.key, "holder", 300) {
		t.Fatal("compression lock refused")
	}
	publish := state.NewCompressionChildPublish(hz.key, hz.key+"-child", "tui", []map[string]any{{"role": "user", "content": "go"}})
	model, holder := "m", "holder"
	publish.Model, publish.CompressionLockHolder = &model, &holder
	if err := db.PublishCompressionChild(context.Background(), publish); err != nil {
		t.Fatal(err)
	}
	if row, _ := db.GetSession(context.Background(), hz.key); row.GetOr("end_reason", nil) != "compression" {
		t.Fatalf("rotation end reason %v", row.GetOr("end_reason", nil))
	}
}

func TestOnlyAnExplicitCloseOnAStillRegisteredSessionIsCleared(t *testing.T) {
	for _, tc := range []struct {
		reason  string
		claimed bool
	}{{"session_reset", false}, {"new_session", false}, {"compression", false}, {"ws_disconnect", false}, {"tui_close", true}} {
		t.Run(tc.reason, func(t *testing.T) {
			hz := newTurnHarness(t, []any{fakellm.Text{Text: "ok"}}, "")
			db := getDB(context.Background())
			stampEnded(t, db, hz.key, tc.reason)
			if !db.TryAcquireCompressionLock(hz.key, "elsewhere", 300) {
				t.Fatal("compression lease refused")
			}
			if tc.claimed && popSessionByID(context.Background(), hz.sid) != hz.session {
				t.Fatal("close did not claim the session")
			}
			if started := submitAndSettle(t, hz, "go"); started == tc.claimed {
				t.Fatalf("started %v", started)
			}
			if row, _ := db.GetSession(context.Background(), hz.key); row.GetOr("end_reason", nil) != tc.reason {
				t.Fatalf("end reason %v", row.GetOr("end_reason", nil))
			}
		})
	}
}
