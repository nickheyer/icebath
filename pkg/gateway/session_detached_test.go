package gateway

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Detached keys keep the chat route namespace and parse back to their session id
func TestDetachedKeyRoundTrip(t *testing.T) {
	if got := detachedKey("agent:fitness:telegram:dm:555", "sid-a"); got != "agent:fitness:detached:sid-a" {
		t.Fatalf("profile key %q", got)
	}
	if got := detachedKey("legacy-route", "sid-b"); got != "agent:main:detached:sid-b" {
		t.Fatalf("fallback namespace %q", got)
	}
	if sid, ok := detachedSessionID("agent:main:detached:sid-b"); !ok || sid != "sid-b" {
		t.Fatalf("parse %q %v", sid, ok)
	}
	if _, ok := detachedSessionID("agent:main:telegram:dm"); ok {
		t.Fatal("chat route parsed as detached")
	}
	if !ownDetachedKey("agent:main:detached:sid-b", "sid-b") || ownDetachedKey("agent:main:detached:sid-b", "sid-c") || ownDetachedKey(nil, "sid-b") {
		t.Fatal("own detached key")
	}
	route := "agent:main:detached:sid-b"
	pinned := &SessionSource{Platform: PlatformTelegram, ChatID: "555", DetachedRoute: &route}
	if lane := laneSource(pinned); lane.DetachedRoute != nil || pinned.DetachedRoute == nil || lane.ChatID != "555" {
		t.Fatal("lane source must drop the pin on a copy")
	}
	plain := &SessionSource{Platform: PlatformTelegram, ChatID: "555"}
	if laneSource(plain) != plain {
		t.Fatal("an undetached source is its own lane source")
	}
}

// A detached source resolves to its own conversation without storing a route, owner hints naming the key
func TestDetachedEntryResolvesConversationWithoutStoring(t *testing.T) {
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "sid-a", Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetSession(ctx, "sid-a")
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	startedAt, _ := row.Get("started_at")
	store := makeTestSessionStore(ctx, t, t.TempDir(), db, DefaultGatewayConfig(ctx))
	key := detachedKey("agent:main:telegram:dm:555", "sid-a")
	source := &SessionSource{Platform: PlatformTelegram, ChatID: "555", ChatType: "dm", DetachedRoute: &key}
	entry, err := store.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if entry.SessionKey != key || entry.SessionID != "sid-a" || entry.Origin != source || entry.Platform != PlatformTelegram || entry.ChatType != "dm" {
		t.Fatalf("entry %+v", entry)
	}
	if want, _ := pyFloat(startedAt); entry.CreatedAt.timestamp()-want > 1e-3 || want-entry.CreatedAt.timestamp() > 1e-3 {
		t.Fatalf("created at %v want %v", entry.CreatedAt.timestamp(), want)
	}
	if store.LookupBySessionKey(ctx, key) != nil {
		t.Fatal("detached entry must never be stored")
	}
	if got := store.ownerKeyForSessionID("sid-a"); got != key {
		t.Fatalf("owner hint %q", got)
	}
	dropOwnerHints(store, key)
	if got := store.ownerKeyForSessionID("sid-a"); got != "" {
		t.Fatalf("hint survived drop %q", got)
	}
}
