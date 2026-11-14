package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	mintedKey   = "20260828_053121_3427a9"
	existingKey = "20260827_224618_956bf8"
)

// Real launch store with a test-model default, synthetic builds and the case's sessions dropped after
func resumeDB(t *testing.T) *state.DB {
	t.Helper()
	h := sessionCaseScope(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  default: test-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := reaperTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	useSharedDB(t, db)
	return db
}

func resumeCall(t *testing.T, params map[string]any) map[string]any {
	t.Helper()
	return handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.resume", "params": params})
}

func assertResumedOK(t *testing.T, resp map[string]any, target string) {
	t.Helper()
	result, isResult := resp["result"].(map[string]any)
	if !isResult || result["resumed"] != target || !truthy(result["session_id"]) {
		t.Fatalf("resume %v", resp)
	}
}

func resumeErrCode(resp map[string]any) any {
	errObj, _ := resp["error"].(map[string]any)
	return errObj["code"]
}

// Count of tui rows the store lists
func tuiRows(t *testing.T, db *state.DB) int {
	t.Helper()
	rows, err := db.ListSessionsRich(context.Background(), state.ListSessionsRichOptions{Source: "tui"})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// test_session_resume_materializes_minted_key.py test_resume_materializes_row_for_minted_key
func TestResumeMaterializesRowForMintedKey(t *testing.T) {
	db := resumeDB(t)
	resp := resumeCall(t, map[string]any{"session_id": mintedKey})
	assertResumedOK(t, resp, mintedKey)
	row, err := db.GetSession(context.Background(), mintedKey)
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	want := home.ProfileNameFor("")
	if want == "" {
		want = currentProfileName(context.Background())
	}
	if row.GetOr("source", nil) != "tui" || row.GetOr("model", nil) != "test-model" || row.GetOr("profile_name", nil) != want {
		t.Fatalf("row source %v model %v profile %v want %q", row.GetOr("source", nil), row.GetOr("model", nil), row.GetOr("profile_name", nil), want)
	}
}

// test_session_resume_materializes_minted_key.py test_resume_existing_row_unaffected
func TestResumeExistingRowUnaffected(t *testing.T) {
	db := resumeDB(t)
	ctx := context.Background()
	if err := db.CreateSession(state.NewSession{ID: existingKey, Source: "tui", Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, existingKey, map[string]any{"role": "user", "content": "hello"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	assertResumedOK(t, resumeCall(t, map[string]any{"session_id": existingKey}), existingKey)
	row, err := db.GetSession(ctx, existingKey)
	if err != nil || row.GetOr("message_count", nil) != int64(1) {
		t.Fatalf("row %v %v", row, err)
	}
}

// test_session_resume_materializes_minted_key.py test_resume_garbage_id_still_4007
func TestResumeGarbageIDStill4007(t *testing.T) {
	db := resumeDB(t)
	if code := resumeErrCode(resumeCall(t, map[string]any{"session_id": "not-a-session"})); code != 4007 {
		t.Fatalf("code %v", code)
	}
	if n := tuiRows(t, db); n != 0 {
		t.Fatalf("rows %d", n)
	}
}

// test_session_resume_materializes_minted_key.py test_resume_8hex_runtime_id_still_4007
func TestResume8hexRuntimeIDStill4007(t *testing.T) {
	db := resumeDB(t)
	if code := resumeErrCode(resumeCall(t, map[string]any{"session_id": "5a81f231"})); code != 4007 {
		t.Fatalf("code %v", code)
	}
	if n := tuiRows(t, db); n != 0 {
		t.Fatalf("rows %d", n)
	}
}

// test_session_resume_materializes_minted_key.py test_resume_lazy_watch_missing_key_still_4007
func TestResumeLazyWatchMissingKeyStill4007(t *testing.T) {
	db := resumeDB(t)
	if code := resumeErrCode(resumeCall(t, map[string]any{"session_id": mintedKey, "lazy": true})); code != 4007 {
		t.Fatalf("code %v", code)
	}
	if row, _ := db.GetSession(context.Background(), mintedKey); row != nil {
		t.Fatalf("row %v", row)
	}
}

// test_session_resume_materializes_minted_key.py test_resume_minted_key_claimed_by_other_profile_fails_closed
func TestResumeMintedKeyClaimedByOtherProfileFailsClosed(t *testing.T) {
	db := resumeDB(t)
	tuigateway.SetSessionRecord("live-other-profile", tuigateway.NewSession(map[string]any{"history": []any{}, "last_active": 0.0,
		"pending_title": "Bot Chat", "pending_hidden": true, "profile_home": "/tmp/other-profile", "running": false,
		"session_key": mintedKey, "source": "desktop"}))
	if code := resumeErrCode(resumeCall(t, map[string]any{"session_id": mintedKey})); code != 4007 {
		t.Fatalf("code %v", code)
	}
	if row, _ := db.GetSession(context.Background(), mintedKey); row != nil {
		t.Fatalf("row %v", row)
	}
}

// test_session_resume_materializes_minted_key.py test_is_server_minted_key_shape
func TestIsServerMintedKeyShape(t *testing.T) {
	for value, expected := range map[string]bool{"20260828_053121_3427a9": true, "20260827_224618_956bf8": true, "5a81f231": false,
		"fcd97d0c": false, "not-a-session": false, "": false, "20260828_053121_3427": false, "20260828_053121_3427a9ZZ": false,
		"Bot Chat": false} {
		if got := isServerMintedKey(value); got != expected {
			t.Fatalf("%q: %v", value, got)
		}
	}
}

// test_resume_session_yolo.py test_session_yolo_toggle_survives_a_backend_restart
func TestSessionYoloToggleSurvivesABackendRestart(t *testing.T) {
	db := resumeDB(t)
	ctx := context.Background()
	t.Cleanup(func() { tools.ClearSession("row-a") })
	session := tuigateway.NewSession(map[string]any{"agent": NewSyntheticHeavyAgent("row-a", "test-model"), "session_key": "row-a"})
	tuigateway.SetSessionRecord("sid", session)
	handleRequest(ctx, map[string]any{"id": "1", "method": "config.set", "params": map[string]any{"session_id": "sid", "key": "yolo"}})
	if !ensureSessionDBRow(ctx, session) {
		t.Fatal("row not persisted")
	}
	row, err := db.GetSession(ctx, "row-a")
	if err != nil || !state.SessionYoloEnabled(objectMap(row)) {
		t.Fatalf("persisted yolo %v %v", row, err)
	}
	tuigateway.SetSessionRecord("sid", nil)
	tools.ClearSession("row-a")
	resp := handleRequest(ctx, map[string]any{"id": "2", "method": "session.resume", "params": map[string]any{"session_id": "row-a"}})
	result := envelopeResult(t, resp)
	if result["session_key"] != "row-a" || !tools.IsSessionYoloEnabled("row-a") {
		t.Fatalf("resume %v yolo %v", result, tools.IsSessionYoloEnabled("row-a"))
	}
	handleRequest(ctx, map[string]any{"id": "3", "method": "config.set", "params": map[string]any{"session_id": result["session_id"], "key": "yolo"}})
	row, err = db.GetSession(ctx, "row-a")
	if tools.IsSessionYoloEnabled("row-a") || err != nil || state.SessionYoloEnabled(objectMap(row)) {
		t.Fatalf("yolo still on %v %v", row, err)
	}
}

// Live lazy record of an ops profile bot chat that has no stored row yet
func liveLazySession(t *testing.T) (string, *tuigateway.Session) {
	t.Helper()
	h := sessionCaseScope(t)
	useSharedDB(t, nil)
	opsHome := filepath.Join(h, "profiles", "ops")
	if err := os.MkdirAll(opsHome, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "live-lazy-1"
	record := tuigateway.NewSession(map[string]any{"history": []any{}, "last_active": 0.0, "pending_title": "Bot Chat",
		"pending_hidden": true, "profile_home": opsHome, "running": false, "session_key": "20260823_000000_abc123", "source": "desktop"})
	tuigateway.SetSessionRecord(sid, record)
	return sid, record
}

// test_resume_live_lazy_session.py test_resume_by_stored_key_reattaches
func TestResumeByStoredKeyReattaches(t *testing.T) {
	sid, record := liveLazySession(t)
	out := lookupMethod("session.resume")(context.Background(), 1, map[string]any{"profile": "ops", "session_id": record.Get("session_key"), "omit_messages": true})
	result := envelopeResult(t, out)
	if result["session_id"] != sid || result["stored_session_id"] != record.Get("session_key") {
		t.Fatalf("result %v", result)
	}
}

// test_resume_live_lazy_session.py test_resume_by_pending_title_reattaches
func TestResumeByPendingTitleReattaches(t *testing.T) {
	sid, _ := liveLazySession(t)
	out := lookupMethod("session.resume")(context.Background(), 1, map[string]any{"profile": "ops", "session_id": "Bot Chat", "omit_messages": true})
	if result := envelopeResult(t, out); result["session_id"] != sid {
		t.Fatalf("result %v", result)
	}
}

// test_resume_live_lazy_session.py test_unscoped_resume_of_profile_session_fails_closed
func TestUnscopedResumeOfProfileSessionFailsClosed(t *testing.T) {
	_, record := liveLazySession(t)
	out := lookupMethod("session.resume")(context.Background(), 1, map[string]any{"session_id": record.Get("session_key"), "omit_messages": true})
	if code := resumeErrCode(out); code != 4007 {
		t.Fatalf("out %v", out)
	}
}

// test_resume_live_lazy_session.py test_unknown_id_still_404s
func TestUnknownIDStill404s(t *testing.T) {
	liveLazySession(t)
	out := lookupMethod("session.resume")(context.Background(), 1, map[string]any{"profile": "ops", "session_id": "ghost-9999", "omit_messages": true})
	if code := resumeErrCode(out); code != 4007 {
		t.Fatalf("out %v", out)
	}
}
