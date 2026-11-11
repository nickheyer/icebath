package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
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

func sessionCall(method string, params map[string]any) map[string]any {
	return lookupMethod(method)(context.Background(), 1, params)
}

// Sorted ids of a session.list answer
func listedIDs(t *testing.T, params map[string]any) []string {
	t.Helper()
	resp := sessionCall("session.list", params)
	result, isResult := resp["result"].(map[string]any)
	if !isResult {
		t.Fatalf("session.list %v", resp)
	}
	var ids []string
	for _, row := range result["sessions"].([]any) {
		ids = append(ids, strOr(row.(map[string]any)["id"]))
	}
	sort.Strings(ids)
	return ids
}

// test_session_list_lineage_rpc.py test_session_list_projects_lineage_root_id
func TestSessionListProjectsLineageRootID(t *testing.T) {
	db := listingDB(t)
	ctx := context.Background()
	t0 := float64(time.Now().Unix()) - 3600
	if err := db.CreateSession(state.NewSession{ID: "root1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	reaperExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0, "root1")
	if _, err := db.AppendMessage(ctx, "root1", map[string]any{"role": "user", "content": "help me refactor auth"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	reaperExec(t, db, "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=?", t0+1800, "compression", "root1")
	if err := db.CreateSession(state.NewSession{ID: "tip1", Source: "cli", ParentSessionID: "root1"}); err != nil {
		t.Fatal(err)
	}
	reaperExec(t, db, "UPDATE sessions SET started_at=? WHERE id=?", t0+1801, "tip1")
	if _, err := db.AppendMessage(ctx, "tip1", map[string]any{"role": "user", "content": "continuing"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	rows := sessionCall("session.list", map[string]any{})["result"].(map[string]any)["sessions"].([]any)
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[strOr(row.(map[string]any)["id"])] = row.(map[string]any)
	}
	tip, found := byID["tip1"]
	if !found {
		t.Fatalf("rows %v", rows)
	}
	if tip["_lineage_root_id"] != "root1" {
		t.Fatalf("lineage root %v", tip["_lineage_root_id"])
	}
}

// test_session_hidden_rpc.py test_session_list_include_hidden
func TestSessionListIncludeHidden(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "plain-chat", "desktop", 1)
	seedCounted(t, db, "bot-chat", "desktop", 2)
	if hidden, err := db.SetSessionHidden(context.Background(), "bot-chat", true); err != nil || !hidden {
		t.Fatalf("hide %v %v", hidden, err)
	}
	if ids := listedIDs(t, map[string]any{}); !slices.Equal(ids, []string{"plain-chat"}) {
		t.Fatalf("default %v", ids)
	}
	if ids := listedIDs(t, map[string]any{"include_hidden": true}); !slices.Equal(ids, []string{"bot-chat", "plain-chat"}) {
		t.Fatalf("all %v", ids)
	}
}

// test_session_hidden_rpc.py test_session_list_hides_internal_sources
func TestSessionListHidesInternalSources(t *testing.T) {
	for _, source := range []string{"oneshot", "kanban", "tool"} {
		t.Run(source, func(t *testing.T) {
			db := listingDB(t)
			seedCounted(t, db, "plain-chat", "desktop", 1)
			seedCounted(t, db, "internal-run", source, 2)
			if ids := listedIDs(t, map[string]any{}); !slices.Equal(ids, []string{"plain-chat"}) {
				t.Fatalf("listed %v", ids)
			}
		})
	}
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

// test_tui_gateway_server.py test_session_most_recent_folds_db_exception_into_null_result
func TestSessionMostRecentFoldsDBExceptionIntoNullResult(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "tui-1", "tui", 1)
	reaperExec(t, db, "ALTER TABLE sessions RENAME TO sessions_locked_away")
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.most_recent", "params": map[string]any{}})
	if _, failed := resp["error"]; failed {
		t.Fatalf("error %v", resp)
	}
	if resp["result"].(map[string]any)["session_id"] != nil {
		t.Fatalf("result %v", resp)
	}
}

// test_tui_gateway_server.py test_session_most_recent_handles_db_unavailable
func TestSessionMostRecentHandlesDBUnavailable(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", blocked)
	useSharedDB(t, nil)
	if result := mostRecent(t); result["session_id"] != nil {
		t.Fatalf("result %v", result)
	}
}

// test_tui_gateway_server.py test_verification_status_returns_recorded_evidence
func TestVerificationStatusReturnsRecordedEvidence(t *testing.T) {
	t.Setenv("ICEBATH_VERIFY_ON_STOP", "1")
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	profileHomeDir := filepath.Join(root, "profiles", "verify")
	project := filepath.Join(t.TempDir(), "project")
	for _, dir := range []string{profileHomeDir, filepath.Join(project, ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"scripts": {"test": "vitest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "pnpm-lock.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tools.VerificationEvidenceRecorder(&tools.Env{Home: profileHomeDir}, "pnpm run test", project, "sid", 0, "green", nil)
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "verification.status",
		"params": map[string]any{"cwd": project, "session_id": "sid", "profile": "verify"}})
	verification := resp["result"].(map[string]any)["verification"].(map[string]any)
	evidence, _ := verification["evidence"].(map[string]any)
	if verification["status"] != "passed" || evidence["canonical_command"] != "pnpm run test" || evidence["scope"] != "full" {
		t.Fatalf("verification %v", verification)
	}
}

// test_tui_gateway_server.py test_verification_status_outside_workspace_is_not_applicable
func TestVerificationStatusOutsideWorkspaceIsNotApplicable(t *testing.T) {
	t.Setenv("ICEBATH_VERIFY_ON_STOP", "1")
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cwd := t.TempDir()
	if facts := tools.ProjectFactsFor(cwd); facts != nil {
		t.Fatalf("precondition: %s has project facts %v", cwd, facts)
	}
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "verification.status",
		"params": map[string]any{"cwd": cwd, "session_id": "sid"}})
	if status := resp["result"].(map[string]any)["verification"].(map[string]any)["status"]; status != "not_applicable" {
		t.Fatalf("status %v", status)
	}
}
