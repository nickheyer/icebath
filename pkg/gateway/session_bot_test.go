package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Seeds a titled Bot Chat row created locally and handed off to a messaging source
func seedHandedOffBotChat(ctx context.Context, t *testing.T, db *state.SessionDB, sid, source, createdSource, sessionKey string) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET title='Bot Chat', source=?, created_source=?, session_key=? WHERE id=?",
			source, createdSource, sessionKey, sid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Title and title source of a session row
func botChatTitle(ctx context.Context, t *testing.T, db *state.SessionDB, sid string) (string, string) {
	t.Helper()
	row, err := db.GetSession(ctx, sid)
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	return botRowText(row, "title"), botRowText(row, "title_source")
}

// A Bot Chat handed off to a lane that moved on frees the canonical title under its platform label
func TestAHandedOffBotChatWhoseLaneMovedOnIsMovedAside(t *testing.T) {
	ctx, db := newTestStateDB(t)
	store := makeTestSessionStore(ctx, t, t.TempDir(), db, DefaultGatewayConfig(ctx))
	entry, err := store.GetOrCreateSession(ctx, &SessionSource{Platform: PlatformTelegram, ChatID: "555", ChatType: "dm"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	seedHandedOffBotChat(ctx, t, db, "bot1", "telegram", "cli", entry.SessionKey)
	if err := releaseSupersededBotChat(ctx, db, store, entry.SessionKey, "bot1"); err != nil {
		t.Fatal(err)
	}
	if title, source := botChatTitle(ctx, t, db, "bot1"); title != "Bot Chat (telegram bot1)" || source != state.TitleSourceUser {
		t.Fatalf("title %q source %q", title, source)
	}
	if moved, err := displaceBotChat(ctx, db, "bot1"); moved || err != nil {
		t.Fatalf("a second displace must find the title moved: %v %v", moved, err)
	}
}

// The lane still holding the chat, an unknown route, a messaging creation or an API source keep the title
func TestABotChatItsLaneStillHoldsKeepsTheTitle(t *testing.T) {
	ctx, db := newTestStateDB(t)
	store := makeTestSessionStore(ctx, t, t.TempDir(), db, DefaultGatewayConfig(ctx))
	entry, err := store.GetOrCreateSession(ctx, &SessionSource{Platform: PlatformTelegram, ChatID: "555", ChatType: "dm"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	seedTitle := func(sid, source, createdSource string) {
		if sid == entry.SessionID {
			if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
				_, err := conn.ExecContext(ctx, "UPDATE sessions SET title='Bot Chat', created_source='cli' WHERE id=?", sid)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return
		}
		seedHandedOffBotChat(ctx, t, db, sid, source, createdSource, "")
	}
	cases := []struct{ route, sid, source, createdSource string }{
		{entry.SessionKey, entry.SessionID, "", ""}, {"agent:main:telegram:dm:999", "unrouted", "telegram", "cli"},
		{entry.SessionKey, "native", "telegram", "telegram"}, {entry.SessionKey, "api", "api_server", "cli"},
	}
	for _, c := range cases {
		seedTitle(c.sid, c.source, c.createdSource)
		if err := releaseSupersededBotChat(ctx, db, store, c.route, c.sid); err != nil {
			t.Fatalf("%s: %v", c.sid, err)
		}
		if title, _ := botChatTitle(ctx, t, db, c.sid); title != "Bot Chat" {
			t.Fatalf("%s moved aside to %q", c.sid, title)
		}
		if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(ctx, "UPDATE sessions SET title=NULL WHERE id=?", c.sid)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := releaseSupersededBotChat(ctx, db, store, entry.SessionKey, "missing"); err != nil {
		t.Fatalf("missing row: %v", err)
	}
}

// Boot releases the titled Bot Chat by its tip route, an API conversation labelled API and long ids clipped
func TestBootReleasesTheHandedOffBotChatAndLabelsAPIConversations(t *testing.T) {
	ctx, db := newTestStateDB(t)
	store := makeTestSessionStore(ctx, t, t.TempDir(), db, DefaultGatewayConfig(ctx))
	entry, err := store.GetOrCreateSession(ctx, &SessionSource{Platform: PlatformTelegram, ChatID: "555", ChatType: "dm"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	seedHandedOffBotChat(ctx, t, db, "bot1", "telegram", "tui", entry.SessionKey)
	if err := releaseSupersededBotChatAtBoot(ctx, db, store); err != nil {
		t.Fatal(err)
	}
	if title, _ := botChatTitle(ctx, t, db, "bot1"); title != "Bot Chat (telegram bot1)" {
		t.Fatalf("boot left %q", title)
	}
	if err := releaseSupersededBotChatAtBoot(ctx, db, store); err != nil {
		t.Fatalf("boot without a titled Bot Chat: %v", err)
	}
	long := strings.Repeat("x", 120)
	for _, sid := range []string{"api1", long} {
		seedHandedOffBotChat(ctx, t, db, sid, "api_server", "cli", "")
		if moved, err := displaceBotChat(ctx, db, sid); !moved || err != nil {
			t.Fatalf("%s: %v %v", sid, moved, err)
		}
	}
	if title, _ := botChatTitle(ctx, t, db, "api1"); title != "Bot Chat (API api1)" {
		t.Fatalf("api title %q", title)
	}
	clipped, _ := botChatTitle(ctx, t, db, long)
	if want := ("Bot Chat (API " + long)[:state.MaxTitleLength-2] + "\u2026)"; clipped != want {
		t.Fatalf("clipped %q", clipped)
	}
}
