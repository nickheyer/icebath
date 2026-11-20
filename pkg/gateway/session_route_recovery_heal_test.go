package gateway

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	_ "modernc.org/sqlite"
)

// Store over dir/sessions pinned to db with the default config, multiplexed when asked
func healStore(ctx context.Context, t *testing.T, dir string, db SessionStoreDB, multiplex bool) *SessionStore {
	t.Helper()
	cfg := DefaultGatewayConfig(ctx)
	cfg.SessionsDir = filepath.Join(dir, "sessions")
	if multiplex {
		on := true
		cfg.MultiplexProfiles = &on
	}
	store := NewSessionStore(ctx, cfg.SessionsDir, cfg, nil)
	store.PinDB(db)
	return store
}

// Persists the entry as the reporter had it, null origin and no platform, then runs the row update
func stripOrigin(ctx context.Context, t *testing.T, db *state.SessionDB, store *SessionStore, key, rowSQL string) {
	t.Helper()
	data := store.LookupBySessionKey(ctx, key).ToDict()
	data.Set("origin", nil)
	data.Delete("platform")
	text, _ := pyjson.Dumps(data)
	if err := db.SaveGatewayRoutingEntry(ctx, key, text, store.routingScope()); err != nil {
		t.Fatal(err)
	}
	if rowSQL == "" {
		return
	}
	conn, err := sql.Open("sqlite", filepath.Join(os.Getenv("ICEBATH_HOME"), "state.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec("UPDATE sessions SET " + rowSQL); err != nil {
		t.Fatal(err)
	}
}

func forumTopic(ctx context.Context, t *testing.T, dir string, db *state.SessionDB) (*SessionSource, *SessionEntry) {
	t.Helper()
	thread, user, name := "600", "111", "Forum"
	source := &SessionSource{Platform: PlatformTelegram, ChatID: "-1001234567890", ChatType: "group", ThreadID: &thread,
		UserID: &user, ChatName: &name}
	entry, err := healStore(ctx, t, dir, db, false).GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		t.Fatal(err)
	}
	return source, entry
}

type flakyRoutingDB struct {
	*state.SessionDB
	calls int
}

func (f *flakyRoutingDB) LoadGatewayRoutingEntries(ctx context.Context, scope string) (map[string]string, error) {
	f.calls++
	if f.calls == 1 {
		return nil, errors.New("database is locked")
	}
	return f.SessionDB.LoadGatewayRoutingEntries(ctx, scope)
}

func TestRoutingEntryOriginHeal(t *testing.T) {
	for _, tc := range []struct{ name, rowSQL string }{{"row-origin", ""}, {"row-columns", "origin_json=NULL"}} {
		t.Run("load_restores_a_missing_origin_from_the_conversation_row/"+tc.name, func(t *testing.T) {
			ctx, db := newTestStateDB(t)
			dir := os.Getenv("ICEBATH_HOME")
			source, entry := forumTopic(ctx, t, dir, db)
			store := healStore(ctx, t, dir, db, false)
			store.ensureLoaded(ctx)
			stripOrigin(ctx, t, db, store, entry.SessionKey, tc.rowSQL)
			reloaded := healStore(ctx, t, dir, db, false)
			origin := reloaded.LookupBySessionKey(ctx, entry.SessionKey).Origin
			if origin == nil || origin.Platform != source.Platform || origin.ChatID != source.ChatID ||
				*origin.ThreadID != *source.ThreadID || *origin.UserID != *source.UserID {
				t.Fatalf("origin %+v", origin)
			}
			if key, err := reloaded.generateSessionKey(ctx, origin, nil); err != nil || key != entry.SessionKey {
				t.Fatalf("key %q %v", key, err)
			}
		})
	}
	t.Run("next_inbound_turn_fills_an_origin_nothing_else_can_restore", func(t *testing.T) {
		ctx, db := newTestStateDB(t)
		dir := os.Getenv("ICEBATH_HOME")
		source, entry := forumTopic(ctx, t, dir, db)
		store := healStore(ctx, t, dir, db, false)
		store.ensureLoaded(ctx)
		stripOrigin(ctx, t, db, store, entry.SessionKey, "origin_json=NULL, chat_id=NULL")
		restored := healStore(ctx, t, dir, db, false)
		if restored.LookupBySessionKey(ctx, entry.SessionKey).Origin != nil {
			t.Fatal("an origin nothing can rebuild was invented")
		}
		if got, err := restored.GetOrCreateSession(ctx, source, false, true); err != nil || got.SessionID != entry.SessionID {
			t.Fatalf("session %v %v", got, err)
		}
		persisted := healStore(ctx, t, dir, db, false).LookupBySessionKey(ctx, entry.SessionKey)
		want, _ := pyjson.Dumps(source.ToDict())
		if persisted.Origin == nil {
			t.Fatal("origin not persisted")
		}
		if got, _ := pyjson.Dumps(persisted.Origin.ToDict()); got != want {
			t.Fatalf("origin %s want %s", got, want)
		}
	})
	t.Run("named_profile_route_heals_from_row_columns_under_multiplex", func(t *testing.T) {
		ctx, db := newTestStateDB(t)
		dir := os.Getenv("ICEBATH_HOME")
		store := healStore(ctx, t, dir, db, true)
		thread, user, profile := "600", "111", "work"
		source := &SessionSource{Platform: PlatformTelegram, ChatID: "-1001", ChatType: "group", ThreadID: &thread,
			UserID: &user, Profile: &profile}
		entry, err := store.GetOrCreateSession(ctx, source, false, true)
		if err != nil || len(entry.SessionKey) < 11 || entry.SessionKey[:11] != "agent:work:" {
			t.Fatalf("entry %v %v", entry, err)
		}
		stripOrigin(ctx, t, db, store, entry.SessionKey, "origin_json=NULL")
		origin := healStore(ctx, t, dir, db, true).LookupBySessionKey(ctx, entry.SessionKey).Origin
		if origin == nil || origin.Profile == nil || *origin.Profile != "work" || *origin.ThreadID != "600" {
			t.Fatalf("origin %+v", origin)
		}
	})
	t.Run("boot_heal_never_lets_a_stale_mirror_overwrite_newer_routing_state", func(t *testing.T) {
		ctx, db := newTestStateDB(t)
		dir := os.Getenv("ICEBATH_HOME")
		_, entry := forumTopic(ctx, t, dir, db)
		key := entry.SessionKey
		stale := entry.ToDict()
		stale.Set("origin", nil)
		mirror := pyjson.NewObject()
		mirror.Set(key, stale)
		text, _ := pyjson.Dumps(mirror)
		if err := os.WriteFile(filepath.Join(dir, "sessions", "sessions.json"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		durable := entry.ToDict()
		durable.Set("suspended", true)
		durable.Set("origin", nil)
		durableText, _ := pyjson.Dumps(durable)
		if err := db.SaveGatewayRoutingEntry(ctx, key, durableText, healStore(ctx, t, dir, db, false).routingScope()); err != nil {
			t.Fatal(err)
		}
		flaky := &flakyRoutingDB{SessionDB: db}
		store := healStore(ctx, t, dir, flaky, false)
		store.LookupBySessionKey(ctx, key)
		recovered := store.LookupBySessionKey(ctx, key)
		if flaky.calls != 2 {
			t.Fatalf("routing loads %d", flaky.calls)
		}
		if !recovered.Suspended || recovered.Origin == nil {
			t.Fatalf("recovered %+v", recovered)
		}
		store.mu.Lock()
		err := store.save(ctx)
		store.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.LoadGatewayRoutingEntries(ctx, store.routingScope())
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := pyjson.Loads(rows[key])
		if v, _ := parsed.(*pyjson.Object).Get("suspended"); v != true {
			t.Fatalf("durable row %s", rows[key])
		}
	})
}
