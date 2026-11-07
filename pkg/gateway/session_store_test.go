package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Temp home with a real state database carrying the gateway routing table
func newTestStateDB(t *testing.T) (context.Context, *state.SessionDB) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	quietHandleHealth(t)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(dir, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// Store over the temp sessions dir pinned to the given database and loaded
func makeTestSessionStore(ctx context.Context, t *testing.T, dir string, db SessionStoreDB, cfg *GatewayConfig) *SessionStore {
	t.Helper()
	store := NewSessionStore(ctx, dir, cfg, nil)
	store.PinDB(db)
	store.ensureLoaded(ctx)
	return store
}

func multiplexConfig(dir string) *GatewayConfig {
	on := true
	return &GatewayConfig{SessionsDir: dir, WriteSessionsJSON: false, MultiplexProfiles: &on, GroupSessionsPerUser: true}
}

func feishuEntry(sessionKey, chatID, profile string) *SessionEntry {
	now := sessionNow()
	e := NewSessionEntry(sessionKey, "sid-"+chatID, now, now)
	e.Platform, e.ChatType = PlatformFeishu, "dm"
	e.Origin = &SessionSource{Platform: PlatformFeishu, ChatID: chatID, ChatType: "dm", Profile: &profile}
	return e
}

// test_purge_profile_routing.py test_purges_deleted_namespace_and_persists_the_drop
func TestPurgesDeletedNamespaceAndPersistsTheDrop(t *testing.T) {
	ctx, db := newTestStateDB(t)
	dir := filepath.Join(t.TempDir(), "sessions")
	store := makeTestSessionStore(ctx, t, dir, db, multiplexConfig(dir))
	store.mu.Lock()
	store.entries.set("agent:gone:feishu:dm:chatA", feishuEntry("agent:gone:feishu:dm:chatA", "chatA", "gone"))
	store.entries.set("agent:keepme:feishu:dm:chatB", feishuEntry("agent:keepme:feishu:dm:chatB", "chatB", "keepme"))
	if err := store.save(ctx); err != nil {
		t.Fatal(err)
	}
	store.mu.Unlock()
	if n, err := store.PurgeProfileRouting(ctx, "gone"); err != nil || n != 1 {
		t.Fatalf("dropped %d %v", n, err)
	}
	if store.entries.has("agent:gone:feishu:dm:chatA") || !store.entries.has("agent:keepme:feishu:dm:chatB") {
		t.Fatal("live index not purged")
	}
	reloaded := makeTestSessionStore(ctx, t, dir, db, multiplexConfig(dir))
	if reloaded.entries.has("agent:gone:feishu:dm:chatA") || !reloaded.entries.has("agent:keepme:feishu:dm:chatB") {
		t.Fatalf("durable index not purged: %v", reloaded.entries.keyList())
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); err == nil {
		t.Fatal("mirror written with write_sessions_json false")
	}
}

// test_purge_profile_routing.py test_namespace_scoped_and_idempotent
func TestPurgeNamespaceScopedAndIdempotent(t *testing.T) {
	ctx, db := newTestStateDB(t)
	dir := filepath.Join(t.TempDir(), "sessions")
	store := makeTestSessionStore(ctx, t, dir, db, multiplexConfig(dir))
	store.entries.set("agent:foo_bar:feishu:dm:chatA", feishuEntry("agent:foo_bar:feishu:dm:chatA", "chatA", "foo_bar"))
	store.entries.set("agent:fooXbar:feishu:dm:chatB", feishuEntry("agent:fooXbar:feishu:dm:chatB", "chatB", "fooXbar"))
	if n, _ := store.PurgeProfileRouting(ctx, "foo_bar"); n != 1 {
		t.Fatalf("first purge %d", n)
	}
	if n, _ := store.PurgeProfileRouting(ctx, "foo_bar"); n != 0 {
		t.Fatalf("second purge %d", n)
	}
	if store.entries.has("agent:foo_bar:feishu:dm:chatA") || !store.entries.has("agent:fooXbar:feishu:dm:chatB") {
		t.Fatal("purge crossed namespaces")
	}
}

// test_rekey_profile_routing.py test_rekeys_old_namespace_and_origin_profile
func TestRekeysOldNamespaceAndOriginProfile(t *testing.T) {
	ctx, db := newTestStateDB(t)
	dir := filepath.Join(t.TempDir(), "sessions")
	store := makeTestSessionStore(ctx, t, dir, db, multiplexConfig(dir))
	store.entries.set("agent:oldname:feishu:dm:chatA", feishuEntry("agent:oldname:feishu:dm:chatA", "chatA", "oldname"))
	store.entries.set("agent:keepme:feishu:dm:chatB", feishuEntry("agent:keepme:feishu:dm:chatB", "chatB", "keepme"))
	if n, err := store.RekeyProfileRouting(ctx, "oldname", "newname"); err != nil || n != 1 {
		t.Fatalf("moved %d %v", n, err)
	}
	if store.entries.has("agent:oldname:feishu:dm:chatA") {
		t.Fatal("old key kept")
	}
	moved := store.entries.get("agent:newname:feishu:dm:chatA")
	if moved == nil || moved.SessionKey != "agent:newname:feishu:dm:chatA" || *moved.Origin.Profile != "newname" {
		t.Fatalf("moved entry %+v", moved)
	}
	if *store.entries.get("agent:keepme:feishu:dm:chatB").Origin.Profile != "keepme" {
		t.Fatal("bystander touched")
	}
	reloaded := makeTestSessionStore(ctx, t, dir, db, multiplexConfig(dir))
	if !reloaded.entries.has("agent:newname:feishu:dm:chatA") || reloaded.entries.has("agent:oldname:feishu:dm:chatA") {
		t.Fatalf("rekey not durable: %v", reloaded.entries.keyList())
	}
}

// test_rekey_profile_routing.py test_does_not_overwrite_existing_new_namespace_key
func TestRekeyDoesNotOverwriteExistingNewNamespaceKey(t *testing.T) {
	ctx, db := newTestStateDB(t)
	dir := filepath.Join(t.TempDir(), "sessions")
	store := makeTestSessionStore(ctx, t, dir, db, multiplexConfig(dir))
	store.entries.set("agent:oldname:feishu:dm:chatA", feishuEntry("agent:oldname:feishu:dm:chatA", "chatA", "oldname"))
	store.entries.set("agent:newname:feishu:dm:chatA", feishuEntry("agent:newname:feishu:dm:chatA", "chatA", "newname"))
	_, err := store.RekeyProfileRouting(ctx, "oldname", "newname")
	if err == nil || !strings.Contains(err.Error(), "routing collision") {
		t.Fatalf("err %v", err)
	}
	if !store.entries.has("agent:oldname:feishu:dm:chatA") || !store.entries.has("agent:newname:feishu:dm:chatA") {
		t.Fatal("collision mutated the index")
	}
}

// test_session_load_bool.py TestSessionLoadBoolCorruption test_bool_entry_skipped_not_fatal
func TestBoolEntrySkippedNotFatal(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	quietHandleHealth(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"_README": "test sentinel", "corrupted_key": true, "valid_key": {"session_key": "agent:main:telegram:dm:123456",
		"session_id": "20260101_120000_abc12345", "created_at": "2026-01-01T12:00:00", "updated_at": "2026-01-01T12:30:00",
		"origin": {"platform": "telegram", "chat_id": "123456", "chat_type": "dm"}}}`
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	store := makeTestSessionStore(ctx, t, dir, nil, &GatewayConfig{GroupSessionsPerUser: true})
	if e := store.entries.get("valid_key"); e == nil || e.SessionID != "20260101_120000_abc12345" {
		t.Fatalf("valid entry not loaded: %v", store.entries.keyList())
	}
	if store.entries.has("corrupted_key") {
		t.Fatal("corrupted entry loaded")
	}
}

func telegramSource() *SessionSource {
	user, name := "u1", "tester"
	return &SessionSource{Platform: PlatformTelegram, ChatID: "c1", ChatType: "dm", UserID: &user, UserName: &name}
}

func pinnedOverrideKey(ctx context.Context, t *testing.T, store *SessionStore) string {
	t.Helper()
	entry, err := store.GetOrCreateSession(ctx, telegramSource(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	override := pyjson.NewObject()
	override.Set("model", "nous/hermes-4")
	override.Set("provider", "nous")
	if err := store.SetModelOverride(ctx, entry.SessionKey, override); err != nil {
		t.Fatal(err)
	}
	if got := store.GetModelOverride(ctx, entry.SessionKey); got == nil || !pyjson.Equal(got, override) {
		t.Fatalf("override %v", got)
	}
	return entry.SessionKey
}

// test_switch_session_keeps_model_override.py both cases
func TestSwitchSessionKeepsModelOverrideAndResetDropsIt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "sessions")
	store := makeTestSessionStore(ctx, t, dir, nil, &GatewayConfig{GroupSessionsPerUser: true, WriteSessionsJSON: true})
	key := pinnedOverrideKey(ctx, t, store)
	switched, err := store.SwitchSession(ctx, key, "other_session_id", nil, true)
	if err != nil || switched == nil || switched.SessionID != "other_session_id" || switched.ModelOverride == nil {
		t.Fatalf("switch %+v %v", switched, err)
	}
	if store.GetModelOverride(ctx, key) == nil {
		t.Fatal("switch dropped the override")
	}
	fresh, err := store.ResetSession(ctx, key, nil)
	if err != nil || fresh == nil || !fresh.IsFreshReset || fresh.ModelOverride != nil {
		t.Fatalf("reset %+v %v", fresh, err)
	}
	if store.GetModelOverride(ctx, key) != nil {
		t.Fatal("reset kept the override")
	}
}

// SessionEntry wire round trip including the tri state yolo and the legacy memory_flushed key
func TestSessionEntryRoundTripAndLegacyKeys(t *testing.T) {
	ctx := context.Background()
	created := pyDatetime{T: time.Date(2026, 1, 2, 3, 4, 5, 6000, time.Local)}
	e := NewSessionEntry("agent:main:telegram:dm:1", "20260102_030405_abcdef12", created, created)
	off := false
	e.Yolo, e.Platform = &off, PlatformTelegram
	d := e.ToDict()
	if v, _ := d.Get("yolo"); v != false {
		t.Fatalf("yolo %v", v)
	}
	if v, _ := d.Get("created_at"); v != "2026-01-02T03:04:05.000006" {
		t.Fatalf("created_at %v", v)
	}
	text, _ := pyjson.Dumps(d)
	parsed, _ := pyjson.Loads(text)
	back, err := SessionEntryFromDict(ctx, parsed.(*pyjson.Object))
	if err != nil || back.Yolo == nil || *back.Yolo || !back.CreatedAt.T.Equal(created.T) {
		t.Fatalf("round trip %+v %v", back, err)
	}
	legacy, _ := pyjson.Loads(`{"session_key": "k", "session_id": "s", "created_at": "2026-01-01T00:00:00",
		"updated_at": "2026-01-01T00:00:00", "yolo": false, "memory_flushed": true, "active_turn_token": "",
		"active_turn_started_at": "2026-01-01T00:00:00"}`)
	old, err := SessionEntryFromDict(ctx, legacy.(*pyjson.Object))
	if err != nil || old.Yolo != nil || !old.ExpiryFinalized || old.ActiveTurnStartedAt != nil {
		t.Fatalf("legacy %+v %v", old, err)
	}
	unsafe, _ := pyjson.Loads(`{"session_key": "k", "session_id": "../x", "created_at": "2026-01-01T00:00:00", "updated_at": "2026-01-01T00:00:00"}`)
	if _, err := SessionEntryFromDict(ctx, unsafe.(*pyjson.Object)); err == nil || !strings.Contains(err.Error(), "directory traversal") {
		t.Fatalf("traversal %v", err)
	}
	if isPathUnsafe("spaces/a/threads/b", false) || !isPathUnsafe("/abs", false) || !isPathUnsafe("C:x", true) {
		t.Fatal("path safety")
	}
}

// Turn markers, crash recovery, resume pending, prompt pins and the suspension reset on the JSONL store
func TestTurnMarkersResumeAndPromptPin(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "sessions")
	store := makeTestSessionStore(ctx, t, dir, nil, &GatewayConfig{GroupSessionsPerUser: true, WriteSessionsJSON: true})
	entry, err := store.GetOrCreateSession(ctx, telegramSource(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	key := entry.SessionKey
	token, err := store.MarkTurnActive(ctx, key)
	if err != nil || token == "" || entry.ActiveTurnStartedAt == nil || !entry.ActiveTurnStartedAt.Aware {
		t.Fatalf("mark %q %v", token, err)
	}
	if ok, _ := store.ClearTurnActive(ctx, key, "stale"); ok {
		t.Fatal("stale token cleared a newer turn")
	}
	if n, _ := store.RecoverInterruptedTurns(ctx, 3600); n != 1 || !entry.ResumePending || *entry.ResumeReason != "restart_interrupted" || entry.ActiveTurnToken != nil {
		t.Fatalf("recover %d %+v", n, entry)
	}
	if ok, _ := store.ClearResumePending(ctx, key); !ok || entry.ResumePending {
		t.Fatal("clear resume")
	}
	if ok, _ := store.SuspendSession(ctx, key); !ok {
		t.Fatal("suspend")
	}
	if ok, _ := store.MarkResumePending(ctx, key, "restart_timeout"); ok {
		t.Fatal("resume armed over a suspension")
	}
	pin, _ := pyjson.Loads(`{"version": 1, "context_key": "ck", "context_prompt": "p", "redact_pii": false}`)
	sid := entry.SessionID
	other := "other"
	if ok, _ := store.SetPromptPin(ctx, key, pin, &other); ok {
		t.Fatal("pin crossed a boundary")
	}
	if ok, _ := store.SetPromptPin(ctx, key, pin, &sid); !ok {
		t.Fatal("pin refused")
	}
	got := store.GetPromptPin(ctx, key, &sid)
	if got == nil || got.Len() != 6 {
		t.Fatalf("pin %v", got)
	}
	reloaded := makeTestSessionStore(ctx, t, dir, nil, &GatewayConfig{GroupSessionsPerUser: true, WriteSessionsJSON: true})
	if p := reloaded.GetPromptPin(ctx, key, nil); p == nil {
		t.Fatal("pin not durable")
	}
	next, err := store.GetOrCreateSession(ctx, telegramSource(), false, true)
	if err != nil || next.SessionID == sid || !next.WasAutoReset || *next.AutoResetReason != "suspended" {
		t.Fatalf("suspended route not replaced %+v %v", next, err)
	}
}
