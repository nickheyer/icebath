package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Launch store of a listing case seeded through the given statements
func listingDB(t *testing.T) *state.DB {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := reaperTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	useSharedDB(t, db)
	return db
}

// Stored row with one counted message, the hidden rpc fixture seed
func seedCounted(t *testing.T, db *state.DB, sid, source string, startedAt float64) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: source}); err != nil {
		t.Fatal(err)
	}
	reaperExec(t, db, "UPDATE sessions SET message_count = 1, started_at = ? WHERE id = ?", startedAt, sid)
}

// Result of session.most_recent through the request dispatcher
func mostRecent(t *testing.T) map[string]any {
	t.Helper()
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.most_recent", "params": map[string]any{}})
	result, isResult := resp["result"].(map[string]any)
	if !isResult {
		t.Fatalf("most_recent %v", resp)
	}
	return result
}

// test_tui_gateway_server.py test_session_most_recent_returns_first_non_denied
func TestSessionMostRecentReturnsFirstNonDenied(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "tool-1", "tool", 100)
	seedCounted(t, db, "tui-1", "tui", 99)
	if _, err := db.SetSessionTitle(context.Background(), "tui-1", "real"); err != nil {
		t.Fatal(err)
	}
	result := mostRecent(t)
	if result["session_id"] != "tui-1" || result["title"] != "real" || result["source"] != "tui" {
		t.Fatalf("result %v", result)
	}
}

// test_tui_gateway_server.py test_session_most_recent_returns_null_when_only_tool_rows
func TestSessionMostRecentReturnsNullWhenOnlyToolRows(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "tool-1", "tool", 1)
	if result := mostRecent(t); result["session_id"] != nil {
		t.Fatalf("result %v", result)
	}
}

// test_tui_gateway_server.py test_session_most_recent_skips_unknown_source_rows
func TestSessionMostRecentSkipsUnknownSourceRows(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "guard-1", "unknown", 101)
	seedCounted(t, db, "tui-1", "tui", 100)
	result := mostRecent(t)
	if result["session_id"] != "tui-1" || result["source"] != "tui" {
		t.Fatalf("result %v", result)
	}
}

// test_tui_gateway_server.py test_session_most_recent_returns_null_when_only_unknown_rows
func TestSessionMostRecentReturnsNullWhenOnlyUnknownRows(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "guard-1", "unknown", 1)
	if result := mostRecent(t); result["session_id"] != nil {
		t.Fatalf("result %v", result)
	}
}

// A store whose listing query fails answers the error with its cause, never a missing session
func TestSessionMostRecentAnswersAFailingQueryWithItsCause(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "tui-1", "tui", 1)
	reaperExec(t, db, "ALTER TABLE sessions RENAME TO sessions_locked_away")
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.most_recent", "params": map[string]any{}})
	failure, _ := resp["error"].(map[string]any)
	data, _ := failure["data"].(map[string]any)
	if details, _ := data["details"].(string); data["cause"] == nil || !strings.Contains(details, "no such table") {
		t.Fatalf("most_recent %v", resp)
	}
}

// A home whose folder cannot be created answers the store error with its cause
func TestSessionMostRecentHandlesDBUnavailable(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", blocked)
	useSharedDB(t, nil)
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.most_recent", "params": map[string]any{}})
	failure, _ := resp["error"].(map[string]any)
	data, _ := failure["data"].(map[string]any)
	if data["cause"] != "store_dir" {
		t.Fatalf("most_recent %v", resp)
	}
}
