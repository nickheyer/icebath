package gatewayrun

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestNormalizeRewritesLobbyThreadToBoundTopic(t *testing.T) {
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.EnableTelegramTopicMode(ctx, state.TelegramTopicMode{ChatID: "555", UserID: "user-1", ProfileName: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "sess-42", Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	if err := db.BindTelegramTopic(ctx, state.TelegramTopicBinding{ChatID: "555", ThreadID: "42", UserID: "user-1",
		SessionKey: "agent:main:telegram:dm:555:42", SessionID: "sess-42", ProfileName: "default"}); err != nil {
		t.Fatal(err)
	}
	r := allocGatewayRunner()
	r.pinSessionDB(state.NewAsyncSessionDB(db))
	empty, user := "", "user-1"
	src := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "555", ChatName: strp("Forum DM"), ChatType: "dm",
		UserID: &user, ThreadID: &empty}
	normalized := r.normalizeSourceForSessionKey(ctx, src)
	if optText(normalized.ThreadID) != "42" {
		t.Fatalf("thread %q", optText(normalized.ThreadID))
	}
	if optText(src.ThreadID) != "" {
		t.Fatal("original source was rewritten")
	}
}
