package gatewayrun

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Telegram private chat source of the topic mode tests on the given thread
func topicModeSource(threadID string) *gateway.SessionSource {
	user, name := "208214988", "tester"
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, UserID: &user, ChatID: "208214988", UserName: &name, ChatType: "dm"}
	if threadID != "" {
		source.ThreadID = &threadID
	}
	return source
}

// Session database with topic mode on for the test chat, one session row per id
func topicModeDB(t *testing.T, sessionIDs ...string) *state.SessionDB {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.EnableTelegramTopicMode(ctx, state.TelegramTopicMode{ChatID: "208214988", UserID: "208214988", ProfileName: "default"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range sessionIDs {
		if err := db.CreateSession(state.NewSession{ID: id, Source: "telegram", UserID: "208214988"}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Runner whose session store and session database are the given topic mode database
func topicModeRunner(t *testing.T, db *state.SessionDB) *GatewayRunner {
	t.Helper()
	ctx := context.Background()
	r := allocGatewayRunner()
	r.config = &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformTelegram: gateway.NewPlatformConfig()}}
	r.config.Platforms[gateway.PlatformTelegram].Enabled = true
	store := gateway.NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), gateway.DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	r.sessionStore = store
	r.pinSessionDB(state.NewAsyncSessionDB(db))
	return r
}

func TestTopicBindingHealSwitchesWithCasOnSnapshotSession(t *testing.T) {
	ctx := context.Background()
	db := topicModeDB(t, "bound-session")
	source := topicModeSource("17585")
	key, err := gateway.BuildSessionKey(ctx, source, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindTelegramTopic(ctx, state.TelegramTopicBinding{ChatID: "208214988", ThreadID: "17585", UserID: "208214988",
		SessionKey: key, SessionID: "bound-session", ManagedMode: "auto", ProfileName: "default"}); err != nil {
		t.Fatal(err)
	}
	r := topicModeRunner(t, db)
	snapshot, err := r.sessionStore.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SessionID == "bound-session" {
		t.Fatal("snapshot already on the bound session")
	}
	healed, err := r.hmwaHealTelegramTopicBinding(ctx, source, snapshot, key)
	if err != nil {
		t.Fatal(err)
	}
	if healed.SessionID != "bound-session" {
		t.Fatalf("heal stayed on %s", healed.SessionID)
	}
	if routed := r.sessionStore.LookupBySessionKey(ctx, key); routed == nil || routed.SessionID != "bound-session" {
		t.Fatalf("route %v", routed)
	}
	stale := &gateway.SessionEntry{SessionKey: key, SessionID: snapshot.SessionID}
	again, err := r.hmwaHealTelegramTopicBinding(ctx, source, stale, key)
	if err != nil {
		t.Fatal(err)
	}
	if again.SessionID != snapshot.SessionID {
		t.Fatalf("a route moved since the snapshot was clobbered by the stale heal: %s", again.SessionID)
	}
	if routed := r.sessionStore.LookupBySessionKey(ctx, key); routed.SessionID != "bound-session" {
		t.Fatalf("route %s", routed.SessionID)
	}
}

func TestFirstMessageInsideTopicRecordsTopicBinding(t *testing.T) {
	ctx := context.Background()
	db := topicModeDB(t, "sess-topic")
	r := topicModeRunner(t, db)
	source := topicModeSource("17585")
	key, err := gateway.BuildSessionKey(ctx, topicModeSource("17585"), true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.recordTelegramTopicBinding(ctx, source, &gateway.SessionEntry{SessionKey: key, SessionID: "sess-topic"}); err != nil {
		t.Fatal(err)
	}
	binding, err := db.GetTelegramTopicBinding(ctx, "208214988", "17585", "default")
	if err != nil || binding == nil {
		t.Fatalf("binding %v %v", binding, err)
	}
	for field, want := range map[string]string{"user_id": "208214988", "session_id": "sess-topic", "session_key": key} {
		if got, _ := binding.Get(field); got != want {
			t.Fatalf("%s = %v", field, got)
		}
	}
}
