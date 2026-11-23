package tuiserver

import (
	"testing"
	"time"

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
