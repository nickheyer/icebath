package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Stored session holding the given messages in order
func storeTranscript(t *testing.T, db *state.DB, sid, parent string, messages ...map[string]any) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "tui", ParentSessionID: parent}); err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if _, err := db.AppendMessage(context.Background(), sid, m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
}

// Compressed root of four rows behind a one row tip, a lineage over the cap with a tip under it
func runawayLineage(t *testing.T, db *state.DB, tip string) {
	t.Helper()
	root := tip + "-root"
	var rows []map[string]any
	for _, text := range []string{"a", "b", "c", "d"} {
		rows = append(rows, map[string]any{"role": "user", "content": text})
	}
	storeTranscript(t, db, root, "", rows...)
	reaperExec(t, db, "UPDATE sessions SET ended_at=started_at+1, end_reason='compression' WHERE id=?", root)
	storeTranscript(t, db, tip, root, map[string]any{"role": "user", "content": "tip"})
	reaperExec(t, db, "UPDATE sessions SET started_at=started_at+2 WHERE id=?", tip)
}
