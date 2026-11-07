package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Real session database recording the threshold every session count probe asks for
type countingStoreDB struct {
	*state.SessionDB
	asked []int
}

func (c *countingStoreDB) SessionCountGE(ctx context.Context, n int) (bool, error) {
	c.asked = append(c.asked, n)
	return true, nil
}

// Store over a fresh directory with the index marked loaded and the database pinned
func loadedTestStore(ctx context.Context, t *testing.T, db SessionStoreDB) *SessionStore {
	t.Helper()
	store := NewSessionStore(ctx, t.TempDir(), DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	store.loaded = true
	return store
}

func TestHasAnySessionsUsesDatabaseCountWhenAvailable(t *testing.T) {
	ctx, db := newTestStateDB(t)
	counting := &countingStoreDB{SessionDB: db}
	store := loadedTestStore(ctx, t, counting)
	store.entries.set("telegram:12345", &SessionEntry{SessionKey: "telegram:12345", SessionID: "s1"})
	if !store.HasAnySessions(ctx) || len(counting.asked) != 1 || counting.asked[0] != 2 {
		t.Fatalf("asked %v", counting.asked)
	}
}

func TestHasAnySessionsFallbackWithoutDatabase(t *testing.T) {
	ctx := context.Background()
	store := loadedTestStore(ctx, t, nil)
	store.entries.set("key1", &SessionEntry{SessionKey: "key1"})
	store.entries.set("key2", &SessionEntry{SessionKey: "key2"})
	if !store.HasAnySessions(ctx) {
		t.Fatal("two entries mean sessions exist")
	}
	store.entries.delete("key2")
	if store.HasAnySessions(ctx) {
		t.Fatal("one entry means no earlier session")
	}
}

func TestLastPromptTokensSessionEntryRoundtrip(t *testing.T) {
	ctx := context.Background()
	entry := &SessionEntry{SessionKey: "test", SessionID: "s1", CreatedAt: sessionNow(), UpdatedAt: sessionNow(), LastPromptTokens: 42000}
	d := entry.ToDict()
	if value, _ := d.Get("last_prompt_tokens"); value != int64(42000) && value != 42000 && value != float64(42000) {
		t.Fatalf("dict value %#v", value)
	}
	restored, err := SessionEntryFromDict(ctx, d)
	if err != nil || restored.LastPromptTokens != 42000 {
		t.Fatalf("restored %+v %v", restored, err)
	}
}

func TestUpdateSessionWithoutTokensDoesNotChangeThem(t *testing.T) {
	ctx := context.Background()
	store := loadedTestStore(ctx, t, nil)
	store.writeSessionsJSON = false
	entry := &SessionEntry{SessionKey: "k1", SessionID: "s1", CreatedAt: sessionNow(), UpdatedAt: sessionNow(), LastPromptTokens: 50000}
	store.entries.set("k1", entry)
	if err := store.UpdateSession(ctx, "k1", nil, true); err != nil {
		t.Fatal(err)
	}
	if entry.LastPromptTokens != 50000 {
		t.Fatalf("tokens %d", entry.LastPromptTokens)
	}
}

// Keeps the handle cache health publish out of the temp home the test removes at cleanup
func quietHandleHealth(t *testing.T) {
	prev := handleCachePublishRuntimeStatus
	handleCachePublishRuntimeStatus = func(RuntimeStatusUpdate) (int, error) { return 0, nil }
	t.Cleanup(func() { handleCachePublishRuntimeStatus = prev })
}

// Slack thread source of the metadata cases
func metadataSlackSource() *SessionSource {
	user, thread := "U123", "123.000"
	return &SessionSource{Platform: PlatformSlack, ChatID: "C123", ChatType: "group", UserID: &user, ThreadID: &thread}
}

func TestSessionMetadataSurvivesReload(t *testing.T) {
	ctx := context.Background()
	quietHandleHealth(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := DefaultGatewayConfig(ctx)
	store := NewSessionStore(ctx, dir, cfg, nil)
	store.PinDB(nil)
	entry, err := store.GetOrCreateSession(ctx, metadataSlackSource(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	key := "slack_thread_watermark:C123:123.000"
	if ok, err := store.SetSessionMetadata(ctx, entry.SessionKey, key, "123.456"); !ok || err != nil {
		t.Fatalf("set %v %v", ok, err)
	}
	reloaded := NewSessionStore(ctx, dir, cfg, nil)
	reloaded.PinDB(nil)
	if got := reloaded.GetSessionMetadata(ctx, entry.SessionKey, key, nil); got != "123.456" {
		t.Fatalf("reloaded %#v", got)
	}
}

func TestMetadataWriteDoesNotTouchActivityClock(t *testing.T) {
	ctx := context.Background()
	quietHandleHealth(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	store := NewSessionStore(ctx, t.TempDir(), DefaultGatewayConfig(ctx), nil)
	store.PinDB(nil)
	entry, err := store.GetOrCreateSession(ctx, metadataSlackSource(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	idle := pyDatetime{T: time.Now().Add(-21 * 24 * time.Hour)}
	store.mu.Lock()
	entry.UpdatedAt = idle
	store.mu.Unlock()
	if ok, err := store.SetSessionMetadata(ctx, entry.SessionKey, "k", "v"); !ok || err != nil {
		t.Fatalf("set %v %v", ok, err)
	}
	store.mu.Lock()
	got := store.entries.get(entry.SessionKey).UpdatedAt
	store.mu.Unlock()
	if !got.T.Equal(idle.T) {
		t.Fatalf("updated at moved to %v", got.T)
	}
}
