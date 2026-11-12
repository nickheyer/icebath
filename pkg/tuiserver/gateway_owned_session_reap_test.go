package tuiserver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestIsGatewayOwnedSourceBuiltinGatewayPlatformsAreOwned(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, src := range []string{"telegram", "discord", "whatsapp", "slack", "signal", "matrix", "mattermost", "bluebubbles", "sms", "email"} {
		if !isGatewayOwnedSource(context.Background(), src) {
			t.Fatal(src)
		}
	}
}

func TestIsGatewayOwnedSourceTuiOwnedSourcesAreNot(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, src := range []string{"tui", "cli", "webui", "desktop", "cron", "subagent", "test", "acp", ""} {
		if isGatewayOwnedSource(context.Background(), src) {
			t.Fatal(src)
		}
	}
}

// Live session over a real agent that owns its store and a stored row of the given source
func realOwnedSession(t *testing.T, source, sessionID string) (string, *tuigateway.Session, *agent.AIAgent) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath_home")
	t.Setenv("ICEBATH_HOME", h)
	dbPath := filepath.Join(h, "state.db")
	db, err := state.NewSessionDB(context.Background(), dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: source}); err != nil {
		t.Fatal(err)
	}
	a, err := agent.NewAIAgent(context.Background(), agent.AIAgentOptions{Model: "m", Provider: "custom", BaseURL: "http://127.0.0.1:9/v1",
		APIKey: "k", APIMode: "chat_completions", SessionID: sessionID, Platform: "tui", QuietMode: true, SkipContextFiles: true,
		SkipMemory: true, SessionDB: db})
	if err != nil {
		t.Fatal(err)
	}
	a.SetOwnsSessionDB(true)
	a.SetEndSessionOnClose(true)
	session := tuigateway.NewSession(map[string]any{"agent": a, "history": []any{map[string]any{"role": "user", "content": "x"}},
		"session_key": sessionID, "profile_home": h, "source": source})
	return dbPath, session, a
}

// Ended at and end reason of a stored row read through a fresh handle
func readRealRow(t *testing.T, dbPath, sessionID string) (any, any) {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	row, err := db.GetSession(context.Background(), sessionID)
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	return row.GetOr("ended_at", nil), row.GetOr("end_reason", nil)
}

func TestViewerTeardownLeavesGatewayOwnedRowOpen(t *testing.T) {
	dbPath, session, a := realOwnedSession(t, "telegram", "gateway-owned-session")
	teardownSession(context.Background(), session, "ws_orphan_reap")
	endedAt, endReason := readRealRow(t, dbPath, "gateway-owned-session")
	if endedAt != nil || endReason != nil || a.EndSessionOnClose() || a.OwnsSessionDB() {
		t.Fatalf("ended_at %v end_reason %v end_on_close %v owns_db %v", endedAt, endReason, a.EndSessionOnClose(), a.OwnsSessionDB())
	}
}

func TestAutomaticReclaimLeavesDesktopRowOpen(t *testing.T) {
	for _, reason := range []string{"ws_orphan_reap", "idle_timeout", "lru_evict"} {
		t.Run(reason, func(t *testing.T) {
			dbPath, session, a := realOwnedSession(t, "desktop", "desktop-"+reason)
			teardownSession(context.Background(), session, reason)
			endedAt, endReason := readRealRow(t, dbPath, "desktop-"+reason)
			if endedAt != nil || endReason != nil || a.EndSessionOnClose() || a.OwnsSessionDB() {
				t.Fatalf("ended_at %v end_reason %v end_on_close %v owns_db %v", endedAt, endReason, a.EndSessionOnClose(), a.OwnsSessionDB())
			}
		})
	}
}

func TestExplicitCloseAndNonDesktopReapStillEndRow(t *testing.T) {
	for _, tc := range [][2]string{{"desktop", "tui_close"}, {"tui", "ws_orphan_reap"}} {
		source, reason := tc[0], tc[1]
		t.Run(source+"-"+reason, func(t *testing.T) {
			dbPath, session, a := realOwnedSession(t, source, source+"-"+reason)
			teardownSession(context.Background(), session, reason)
			endedAt, endReason := readRealRow(t, dbPath, source+"-"+reason)
			if endedAt == nil || endReason != reason || !a.EndSessionOnClose() {
				t.Fatalf("ended_at %v end_reason %v end_on_close %v", endedAt, endReason, a.EndSessionOnClose())
			}
		})
	}
}
