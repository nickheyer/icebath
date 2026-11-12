package tuiserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session store at path with the given sessions created, closed at cleanup
func rosterStore(t *testing.T, path string, sessions ...state.NewSession) *state.DB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, session := range sessions {
		if err := db.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Real background process spawned under a route and parent conversation, killed at cleanup
func rosterProcess(t *testing.T, sessionKey, parent string) {
	t.Helper()
	env := &tools.Env{Home: t.TempDir(), Values: &sync.Map{}}
	tools.SetSessionVars(env, tools.SessionVars{SessionID: parent, SessionKey: sessionKey})
	registry := tools.DefaultProcessRegistry()
	proc, err := registry.SpawnLocal(context.Background(), env, "sleep 90", tools.SpawnLocalOptions{Cwd: t.TempDir(), SessionKey: sessionKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		registry.KillProcess(context.Background(), env, proc.ID, "test", true)
		registry.ForgetFinished(proc.ID)
	})
}

// Roster of a profile through the real request path keyed by session id
func rosterCall(t *testing.T, profile string) map[string]map[string]any {
	t.Helper()
	params := map[string]any{}
	if profile != "" {
		params["profile"] = profile
	}
	resp := handleRequest(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "subagent.roster", "params": params})
	result, isResult := resp["result"].(map[string]any)
	if !isResult {
		t.Fatalf("roster %v", resp)
	}
	out := map[string]map[string]any{}
	for _, entry := range result["sessions"].([]any) {
		row := entry.(map[string]any)
		out[row["session_id"].(string)] = row
	}
	return out
}

// Launch store holding parent-a with an empty live child registry
func rosterFixture(t *testing.T) *state.DB {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := rosterStore(t, filepath.Join(t.TempDir(), "state.db"), state.NewSession{ID: "parent-a", Source: "tui"})
	useSharedDB(t, db)
	agent.ClearActiveSubagents()
	t.Cleanup(agent.ClearActiveSubagents)
	return db
}

// test_subagent_roster.py test_roster_reports_live_children_without_an_attached_session
func TestRosterReportsLiveChildrenWithoutAnAttachedSession(t *testing.T) {
	rosterFixture(t)
	agent.RegisterSubagent(map[string]any{"subagent_id": "c1", "depth": 0, "owner_agent_session_id": "parent-a", "status": "running"})
	agent.RegisterSubagent(map[string]any{"subagent_id": "c2", "depth": 0, "owner_agent_session_id": "parent-a", "status": "running"})
	agent.RegisterSubagent(map[string]any{"subagent_id": "g1", "depth": 1, "owner_agent_session_id": "child-x", "status": "running"})
	want := map[string]map[string]any{"parent-a": {"session_id": "parent-a", "subagents": 2, "background": 0}}
	if got := rosterCall(t, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("roster %v", got)
	}
}

// test_subagent_roster.py test_roster_omits_conversations_stored_in_another_profile
func TestRosterOmitsConversationsStoredInAnotherProfile(t *testing.T) {
	rosterFixture(t)
	agent.RegisterSubagent(map[string]any{"subagent_id": "c1", "depth": 0, "owner_agent_session_id": "other-profile-sid"})
	got, err := json.Marshal(rosterCall(t, ""))
	if err != nil || string(got) != "{}" {
		t.Fatalf("roster %s %v", got, err)
	}
}

// test_subagent_roster.py test_roster_resolves_a_background_process_route_key_to_its_stored_conversation
func TestRosterResolvesABackgroundProcessRouteKeyToItsStoredConversation(t *testing.T) {
	db := rosterFixture(t)
	route := "agent:main:local:dm:desktop-chat-1"
	if err := db.CreateSession(state.NewSession{ID: "local-abc123", Source: "local", SessionKey: route}); err != nil {
		t.Fatal(err)
	}
	rosterProcess(t, route, "")
	want := map[string]map[string]any{"local-abc123": {"session_id": "local-abc123", "subagents": 0, "background": 1}}
	if got := rosterCall(t, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("roster %v", got)
	}
}

const rosterRoute = "agent:main:telegram:dm:42"

// Launch and bot2 profile stores holding identical legacy routes, the launch store shared
func rosterTwoProfiles(t *testing.T) {
	t.Helper()
	launch := filepath.Join(t.TempDir(), "launch-home")
	bot2 := filepath.Join(launch, "profiles", "bot2")
	mkdirAll(t, bot2)
	t.Setenv("ICEBATH_HOME", launch)
	prevHome := wsTestSwapLaunchHome(launch)
	prevMultiplex := provider.IsMultiplexActive(context.Background())
	provider.SetMultiplexActive(false)
	servedProfileMu.Lock()
	prevServed := servedProfileHomes
	servedProfileHomes = map[string]bool{}
	servedProfileMu.Unlock()
	prevOpen := OpenSessionDBAtPath
	OpenSessionDBAtPath = func(ctx context.Context, dbPath string, readOnly bool) (*state.DB, error) {
		return state.NewSessionDB(ctx, dbPath, readOnly)
	}
	t.Cleanup(func() {
		provider.SetMultiplexActive(prevMultiplex)
		OpenSessionDBAtPath = prevOpen
		wsTestSwapLaunchHome(prevHome)
		servedProfileMu.Lock()
		servedProfileHomes = prevServed
		servedProfileMu.Unlock()
	})
	launchDB := rosterStore(t, filepath.Join(launch, "state.db"), state.NewSession{ID: "default-conv", Source: "telegram", SessionKey: rosterRoute})
	bot2DB, err := state.NewSessionDB(context.Background(), filepath.Join(bot2, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []state.NewSession{{ID: "bot2-legacy", Source: "telegram", SessionKey: rosterRoute},
		{ID: "bot2-conv", Source: "telegram", SessionKey: "agent:bot2:telegram:dm:42"}} {
		if err := bot2DB.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	bot2DB.Close()
	useSharedDB(t, launchDB)
	agent.ClearActiveSubagents()
	t.Cleanup(agent.ClearActiveSubagents)
}

// Background counts of a profile's roster keyed by session id
func rosterBackground(t *testing.T, profile string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for sid, row := range rosterCall(t, profile) {
		out[sid] = row["background"]
	}
	return out
}

// test_subagent_roster_profiles.py test_a_stored_parent_id_is_authoritative_across_profiles
func TestAStoredParentIdIsAuthoritativeAcrossProfiles(t *testing.T) {
	rosterTwoProfiles(t)
	rosterProcess(t, rosterRoute, "default-conv")
	if got := rosterBackground(t, "bot2"); len(got) != 0 {
		t.Fatalf("bot2 %v", got)
	}
	if got := rosterBackground(t, ""); !reflect.DeepEqual(got, map[string]any{"default-conv": 1}) {
		t.Fatalf("launch %v", got)
	}
	if got := rosterBackground(t, "bot2"); len(got) != 0 {
		t.Fatalf("bot2 after launch %v", got)
	}
}

// test_subagent_roster_profiles.py test_route_fallback_is_fenced_to_the_profile_namespace
func TestRouteFallbackIsFencedToTheProfileNamespace(t *testing.T) {
	rosterTwoProfiles(t)
	rosterProcess(t, rosterRoute, "")
	rosterProcess(t, "agent:bot2:telegram:dm:42", "")
	if got := rosterBackground(t, "bot2"); !reflect.DeepEqual(got, map[string]any{"bot2-conv": 1}) {
		t.Fatalf("bot2 %v", got)
	}
	if got := rosterBackground(t, ""); !reflect.DeepEqual(got, map[string]any{"default-conv": 1}) {
		t.Fatalf("launch %v", got)
	}
}
