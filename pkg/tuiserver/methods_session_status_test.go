package tuiserver_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Status report hooks assigned the way cmd/icebath wiring assigns them
func useStatusReport(t *testing.T) {
	t.Helper()
	saved := tuiserver.StatusReport
	tuiserver.StatusReport.Fields, tuiserver.StatusReport.Lines = app.TUIStatusFields, app.TUIStatusLines
	t.Cleanup(func() { tuiserver.StatusReport = saved })
}

// Store at dir/state.db holding the status row under the title
func statusStore(t *testing.T, dir, title string) *state.DB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(dir, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "status-row", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionTitle(context.Background(), "status-row", title); err != nil {
		t.Fatal(err)
	}
	return db
}

func statusOutput(t *testing.T, sid string) string {
	t.Helper()
	resp := tuiserver.LookupMethod("session.status")(context.Background(), "1", map[string]any{"session_id": sid})
	result, isResult := resp["result"].(map[string]any)
	if !isResult {
		t.Fatalf("resp %v", resp)
	}
	return result["output"].(string)
}
