package gateway

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

func swapAcquireSessionDB(t *testing.T, fn func(ctx context.Context, path string) (*state.SessionDB, error)) *[]RuntimeStatusUpdate {
	t.Helper()
	old, oldPublish := acquireSessionDB, handleCachePublishRuntimeStatus
	writes := &[]RuntimeStatusUpdate{}
	acquireSessionDB = fn
	handleCachePublishRuntimeStatus = func(_ context.Context, u RuntimeStatusUpdate) (int, error) {
		*writes = append(*writes, u)
		return 0, nil
	}
	t.Cleanup(func() { acquireSessionDB, handleCachePublishRuntimeStatus = old, oldPublish })
	return writes
}

func TestSessionStoreInitPrimesActiveScopeAndRoutingHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	var opened []string
	writes := swapAcquireSessionDB(t, func(ctx context.Context, path string) (*state.SessionDB, error) {
		opened = append(opened, path)
		return nil, errors.New("disk unavailable")
	})
	store := NewSessionStore(ctx, filepath.Join(dir, "sessions"), nil, nil)
	if !slices.Equal(opened, []string{state.ResolveDefaultDBPath(ctx)}) {
		t.Fatalf("opened = %v", opened)
	}
	if store.routingHome != home.Dir(ctx) {
		t.Fatalf("routing home = %q", store.routingHome)
	}
	if !store.writeSessionsJSON || store.dbPinned || store.RouteNamed != nil || store.loaded || store.routingDBLoaded {
		t.Fatalf("unexpected initial state %+v", store)
	}
	if got := store.dbHandleCache.LastError(state.ResolveDefaultDBPath(ctx)); !strings.Contains(got, "disk unavailable") {
		t.Fatalf("failed open not recorded for backoff, last error %q", got)
	}
	handleHealthMu.Lock()
	health := handleHealthStates[store.dbHandleCache.healthSource][state.ResolveDefaultDBPath(ctx)]
	handleHealthMu.Unlock()
	if health != "unavailable" || len(*writes) != 1 || !reflect.DeepEqual(slices.Collect(maps.Keys((*writes)[0].Fields)), []string{"session_store"}) {
		t.Fatalf("health %q writes %+v", health, *writes)
	}
	if off := NewSessionStore(ctx, dir, &GatewayConfig{WriteSessionsJSON: false}, nil); off.writeSessionsJSON {
		t.Fatal("write_sessions_json false ignored")
	}
}

func TestSessionStoreInitRaisesLiveSystemGuard(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	guard := errors.New("live-system guard: test attempted to open production state.db")
	swapAcquireSessionDB(t, func(ctx context.Context, path string) (*state.SessionDB, error) {
		return nil, guard
	})
	defer func() {
		if r := recover(); r != guard {
			t.Fatalf("recovered %v, want the live-system guard", r)
		}
	}()
	NewSessionStore(context.Background(), dir, nil, nil)
	t.Fatal("construction swallowed the live-system guard")
}

func TestBareSessionStoreCreatesOptionalMapsOnUse(t *testing.T) {
	store := &SessionStore{}
	if !store.claimLegacySlackKey("agent:main:slack:dm:D1") || store.claimLegacySlackKey("agent:main:slack:dm:D1") {
		t.Fatal("legacy Slack key claimed twice on a bare store")
	}
	store.markSpooledDropSessions(map[string]bool{"s1": true})
	if !store.spooledDropSessions["s1"] {
		t.Fatal("spooled drop session not recorded on a bare store")
	}
	hints := lazyMap(&store.sessionOwnerHints)
	hints["sid"] = "key"
	if again := lazyMap(&store.sessionOwnerHints); again["sid"] != "key" {
		t.Fatal("second read replaced the existing map")
	}
}

func TestBareSessionStoreReroutesCompressionClosedParent(t *testing.T) {
	ctx, db := newTestStateDB(t)
	createTestSession(t, db, state.NewSession{ID: "parent", Source: "telegram"})
	endCompression(t, db, "parent", "compression")
	createTestSession(t, db, state.NewSession{ID: "child", Source: "telegram", ParentSessionID: "parent"})
	replaceTestMessages(ctx, t, db, "child", map[string]any{"role": "user", "content": "summary"})
	store := &SessionStore{entries: newSessionEntries(), loaded: true,
		dirtyTranscripts: map[string]*transcriptQueue{}, transcriptAppendFailures: map[string]int{}}
	store.PinDB(db)
	store.entries.set("route", &SessionEntry{SessionKey: "route", SessionID: "parent"})

	appendTranscript(ctx, t, store, "parent", "assistant", "routed to child")

	if got := store.entries.get("route").SessionID; got != "child" {
		t.Fatalf("route = %q", got)
	}
	if store.sessionOwnerHints == nil || store.transcriptReroutes["parent"] != "child" {
		t.Fatalf("lazy maps not created, owner hints %v reroutes %v", store.sessionOwnerHints, store.transcriptReroutes)
	}
	if _, kept := store.sessionOwnerHints["child"]; kept {
		t.Fatal("owner hint kept after the reroute was published")
	}
	if _, ok := store.dirtyTranscripts["parent"]; ok {
		t.Fatal("parent queue left behind")
	}
	if got := transcriptContents(ctx, t, db, "child"); !slices.Equal(got, []any{"summary", "routed to child"}) {
		t.Fatalf("child rows = %v", got)
	}
}
