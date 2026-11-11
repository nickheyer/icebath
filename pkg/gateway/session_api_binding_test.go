package gateway

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Canonical authority over a fresh profile state database, the owner fixture of the hermes gateway tests
func newAPIOwner(t *testing.T) (context.Context, *SessionAuthority, *recoverTestRunner) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	epoch, err := db.BeginRuntimeEpoch(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	runner := &recoverTestRunner{store: store, adapters: map[Platform]PlatformAdapter{}, profiles: map[string]map[Platform]PlatformAdapter{}}
	a := NewSessionAuthority(runner, "default", "first", db, epoch)
	runner.launch = a
	return ctx, a, runner
}

func apiBind(t *testing.T, ctx context.Context, a *SessionAuthority, sid string) SessionRef {
	t.Helper()
	ref, err := bindAPISession(ctx, a, sid, apiBindOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func apiCreate(t *testing.T, a *SessionAuthority, sid, source string) {
	t.Helper()
	if err := a.DB.CreateSession(state.NewSession{ID: sid, Source: source}); err != nil {
		t.Fatal(err)
	}
}

func apiAppend(t *testing.T, ctx context.Context, a *SessionAuthority, sid, text string) {
	t.Helper()
	if _, err := a.DB.AppendMessage(ctx, sid, map[string]any{"role": "user", "content": text}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

func apiRouteTarget(a *SessionAuthority, route string) string {
	store := a.Runner.SessionStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	if entry := store.entries.get(route); entry != nil {
		return entry.SessionID
	}
	return ""
}

// Leaves what the stale route heal leaves, a fresh session row owning the caller's API route
func mintOnRoute(t *testing.T, ctx context.Context, a *SessionAuthority, caller string) string {
	t.Helper()
	store := a.Runner.SessionStore()
	user := "api"
	route, err := store.generateSessionKey(ctx, &SessionSource{Platform: PlatformAPIServer, ChatID: caller, UserID: &user, ChatType: "dm"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	apiCreate(t, a, "minted", "api_server")
	now := sessionNow()
	minted := NewSessionEntry(route, "minted", now, now)
	minted.Platform = PlatformAPIServer
	text, err := pyjson.Dumps(minted.ToDict())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('',?,?,0) "+
			"ON CONFLICT(scope,session_key) DO UPDATE SET entry_json=excluded.entry_json", route, text)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.ensureLoadedLocked(ctx)
	store.entries.set(route, minted)
	store.mu.Unlock()
	return route
}

func TestBindTakesTheRouteBackFromAnEmptyMintedSession(t *testing.T) {
	for name, boundBefore := range map[string]bool{"unbound": false, "bound_before": true} {
		t.Run(name, func(t *testing.T) {
			ctx, owner, _ := newAPIOwner(t)
			if boundBefore {
				apiBind(t, ctx, owner, "api-stable")
			}
			route := mintOnRoute(t, ctx, owner, "api-stable")
			apiBind(t, ctx, owner, "api-stable")
			if target := apiRouteTarget(owner, route); target != "api-stable" {
				t.Fatalf("route names %q", target)
			}
		})
	}
}

func TestAFinalizedBoundRowKeepsItsRouteAcrossLaterRuns(t *testing.T) {
	ctx, owner, _ := newAPIOwner(t)
	ref := apiBind(t, ctx, owner, "api-stable")
	live, _ := owner.live("api-stable")
	source := live.Source
	if err := owner.DB.EndSession("api-stable", "cli_close"); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []string{"two", "three"} {
		if again := apiBind(t, ctx, owner, "api-stable"); again != ref {
			t.Fatalf("bind answered %v", again)
		}
		entry, err := owner.Runner.SessionStore().GetOrCreateSession(ctx, source, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if entry.SessionID != "api-stable" {
			t.Fatalf("turn %s routed to %q", turn, entry.SessionID)
		}
		apiAppend(t, ctx, owner, "api-stable", turn)
	}
}

func TestBindTakesBackARouteMainForkedOntoAUsedReplacement(t *testing.T) {
	ctx, owner, _ := newAPIOwner(t)
	ref := apiBind(t, ctx, owner, "S")
	live, _ := owner.live("S")
	route := live.Route
	store := owner.Runner.SessionStore()
	apiCreate(t, owner, "minted", "api_server")
	apiAppend(t, ctx, owner, "minted", "two")
	now := sessionNow()
	replacement := NewSessionEntry(route, "minted", now, now)
	replacement.Platform, replacement.Origin = PlatformAPIServer, live.Source
	store.mu.Lock()
	store.ensureLoadedLocked(ctx)
	store.entries.set(route, replacement)
	store.mu.Unlock()
	if err := store.saveEntries(ctx); err != nil {
		t.Fatal(err)
	}
	if again := apiBind(t, ctx, owner, "S"); again != ref {
		t.Fatalf("bind answered %v", again)
	}
	if target := apiRouteTarget(owner, route); target != "S" {
		t.Fatalf("route names %q", target)
	}
	messages, err := owner.DB.GetMessages(ctx, "minted", state.GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var contents []any
	for _, m := range messages {
		content, _ := m.Get("content")
		contents = append(contents, content)
	}
	if !reflect.DeepEqual(contents, []any{"two"}) {
		t.Fatalf("replacement transcript %v", contents)
	}
}

func TestBindStillRefusesARouteHeldByABoundSession(t *testing.T) {
	ctx, owner, _ := newAPIOwner(t)
	mintOnRoute(t, ctx, owner, "api-stable")
	apiAppend(t, ctx, owner, "minted", "a conversation of its own")
	if err := owner.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", state.LocalAPIBindingPrefix+"minted", "{}")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := bindAPISession(ctx, owner, "api-stable", apiBindOptions{})
	recoverRefused(t, err, "admission_conflict")
}

func TestPrivateApiBindingRestoresExactRoute(t *testing.T) {
	ctx, owner, _ := newAPIOwner(t)
	apiCreate(t, owner, "api-stored", "api_server")
	ref := apiBind(t, ctx, owner, "api-stored")
	first, _ := owner.live(ref.SessionID)
	if first.Source.Platform != PlatformAPIServer {
		t.Fatalf("platform %v", first.Source.Platform)
	}
	store := owner.Runner.SessionStore()
	entry, err := store.GetOrCreateSession(ctx, first.Source, false, true)
	if err != nil || entry.SessionID != "api-stored" {
		t.Fatalf("routed to %v, %v", entry, err)
	}
	owner.mu.Lock()
	clear(owner.Sessions)
	owner.mu.Unlock()
	if err := restoreAPISession(ctx, owner, "api-stored"); err != nil {
		t.Fatal(err)
	}
	restored, ok := owner.live(ref.SessionID)
	if !ok || restored.Route != first.Route {
		t.Fatalf("restored %v", restored)
	}
	entry, err = store.GetOrCreateSession(ctx, restored.Source, false, true)
	if err != nil || entry.SessionID != ref.SessionID {
		t.Fatalf("routed to %v, %v", entry, err)
	}
	apiCreate(t, owner, "foreign-native", "telegram")
	_, err = bindAPISession(ctx, owner, "foreign-native", apiBindOptions{})
	recoverRefused(t, err, "permission_denied")
	if recoverLive(owner, "foreign-native") {
		t.Fatal("a foreign native row was bound")
	}
}

func TestPrivateApiBindingAdoptsMatchingLegacyDeclaredKey(t *testing.T) {
	ctx, owner, _ := newAPIOwner(t)
	sessionID, declaredKey := "legacy-api-session", "webui:legacy-api-session"
	apiCreate(t, owner, sessionID, "api_server")
	if err := owner.DB.RecordGatewaySessionPeer(ctx, sessionID, state.GatewayPeer{Source: "api_server", SessionKey: &declaredKey}); err != nil {
		t.Fatal(err)
	}
	ref, err := bindAPISession(ctx, owner, sessionID, apiBindOptions{DeclaredKey: declaredKey})
	if err != nil {
		t.Fatal(err)
	}
	row, err := owner.DB.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	live, _ := owner.live(ref.SessionID)
	if key, _ := row.Get("session_key"); key != live.Route {
		t.Fatalf("row key %v, route %q", key, live.Route)
	}
	declared, found, err := declaredAPISession(ctx, owner.DB, declaredKey)
	if err != nil || !found || declared != sessionID {
		t.Fatalf("declared %q %v %v", declared, found, err)
	}
	_, err = bindAPISession(ctx, owner, "other-api-session", apiBindOptions{DeclaredKey: declaredKey})
	recoverRefused(t, err, "admission_conflict")
}

func TestKeepPassedFencesReportsAWalkItCouldNotPin(t *testing.T) {
	ctx, owner, _ := newAPIOwner(t)
	derive := func(n int) string { return apiGenerationID("root-x", n) }
	passed := []string{"root-x", derive(1)}
	if err := owner.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		return state.RetireRoutes(ctx, conn, passed, passed)
	}); err != nil {
		t.Fatal(err)
	}
	keep := func() bool {
		t.Helper()
		kept := false
		if err := owner.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
			var err error
			kept, err = state.KeepPassedFences(ctx, conn, derive, derive(2))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return kept
	}
	if !keep() {
		t.Fatal("a walk over two fenced generations did not hold")
	}
	for _, sid := range passed {
		text, found, err := owner.DB.GetMeta(ctx, state.RetiredPrefix+sid)
		if err != nil || !found {
			t.Fatalf("fence of %s: %v %v", sid, found, err)
		}
		marker, err := loadsObject(text)
		if err != nil {
			t.Fatal(err)
		}
		if _, expires := marker.Get("expires"); expires {
			t.Fatalf("the fence of %s still expires", sid)
		}
	}
	if err := owner.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=?", state.RetiredPrefix+passed[1])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if keep() {
		t.Fatal("a walk past a reclaimed fence still holds")
	}
	_, err := bindAPISession(ctx, owner, derive(2), apiBindOptions{WalkedFrom: "root-x"})
	recoverRefused(t, err, "chain_moved")
}

func TestEffortFieldsOfAResolvedRoute(t *testing.T) {
	plain := func(o *pyjson.Object) map[string]any { return pyjson.Plain(o).(map[string]any) }
	off := plain(effortFields(map[string]any{"enabled": false, "effort": "high"}, "", "m", nil, nil, nil))
	if off["reasoning_effort"] != "none" || off["reasoning_effort_wire"] != "" || off["service_tier"] != "" || off["fast"] != false {
		t.Fatalf("disabled reasoning %v", off)
	}
	unset := plain(effortFields(nil, "", "m", nil, nil, nil))
	if unset["reasoning_effort"] != "" || unset["reasoning_effort_wire"] != "" || unset["fast"] != false {
		t.Fatalf("no reasoning config %v", unset)
	}
	high := plain(effortFields(map[string]any{"enabled": true, "effort": "high"}, "standard", "m", "custom", "http://x.invalid/v1", nil))
	if high["reasoning_effort"] != "high" || high["reasoning_effort_wire"] == "" || high["service_tier"] != "standard" || high["fast"] != false {
		t.Fatalf("high effort %v", high)
	}
}
