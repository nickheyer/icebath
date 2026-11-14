package tuiserver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Session store holding one ended session with an old exchange
func finalizedDB(t *testing.T) *state.DB {
	t.Helper()
	ctx := context.Background()
	h := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", h)
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "finalized", Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]any{{"role": "user", "content": "old ask", "timestamp": 100.0},
		{"role": "assistant", "content": "old answer", "timestamp": 101.0}} {
		if _, err := db.AppendMessage(ctx, "finalized", m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.EndSession("finalized", "agent_close"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestReopenIfFinalizedLeavesLiveRowsUntouched(t *testing.T) {
	ctx := context.Background()
	db := finalizedDB(t)
	if err := db.CreateSession(state.NewSession{ID: "live", Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "live", map[string]any{"role": "user", "content": "ask", "timestamp": 100.0}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"live", "missing-entirely"} {
		if err := reopenIfFinalized(ctx, db, sid); err != nil {
			t.Fatal(err)
		}
	}
	row, err := db.GetSession(ctx, "live")
	if err != nil || row == nil {
		t.Fatalf("row %v err %v", row, err)
	}
	if row.Vals["ended_at"] != nil || row.Vals["end_reason"] != nil {
		t.Fatalf("live row changed %v", row.Vals)
	}
	ended, err := db.GetSession(ctx, "finalized")
	if err != nil || ended.Vals["ended_at"] == nil {
		t.Fatalf("finalized row %v err %v", ended, err)
	}
}
