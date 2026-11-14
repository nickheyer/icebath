package tuiserver

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session of the stop hook cases, running or idle
func stopHookSession(running bool) *tuigateway.Session {
	return tuigateway.NewSession(map[string]any{"running": running, "queued_prompt": nil, "session_key": "agent:main:tui:dm:s1",
		"agent": nil, "_run_thread": nil})
}

// Registers a hook on the manager of the home ctx names, seeing the caller kwargs without the manager stamp
func recordHook(ctx context.Context, hookName string, fn func(ctx context.Context, kwargs map[string]any) error) {
	manager := plugins.GetPluginManager(ctx)
	manager.MarkDiscoveredForTests()
	plugins.NewContext(&plugins.Manifest{Name: "stop-hook-test"}, manager).RegisterHook(hookName,
		func(ctx context.Context, kwargs map[string]any) (any, error) {
			caller := maps.Clone(kwargs)
			if caller["telemetry_schema_version"] == plugins.ObserverSchemaVersion {
				delete(caller, "telemetry_schema_version")
			}
			return nil, fn(ctx, caller)
		}, false)
}

// Fresh launch home with its plugin managers reset around the case
func stopHookHome(t *testing.T) string {
	t.Helper()
	launch := filepath.Join(t.TempDir(), "icebath_home")
	if err := os.MkdirAll(launch, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launch)
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	return launch
}

func TestInterruptRunningTurnFiresAgentLoopStopped(t *testing.T) {
	stopHookHome(t)
	var calls []map[string]any
	recordHook(context.Background(), "agent_loop_stopped", func(_ context.Context, kwargs map[string]any) error {
		calls = append(calls, kwargs)
		return nil
	})
	interruptSessionTurn(context.Background(), "s1", stopHookSession(true), "", false)
	want := map[string]any{"session_key": "agent:main:tui:dm:s1", "platform": "tui", "reason": "user_stop", "invalidation_reason": "session_interrupt"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("hook calls %v", calls)
	}
}

func TestInterruptIdleSessionDoesNotFireHook(t *testing.T) {
	stopHookHome(t)
	var calls []map[string]any
	recordHook(context.Background(), "agent_loop_stopped", func(_ context.Context, kwargs map[string]any) error {
		calls = append(calls, kwargs)
		return nil
	})
	interruptSessionTurn(context.Background(), "s1", stopHookSession(false), "", false)
	if len(calls) != 0 {
		t.Fatalf("hook calls %v", calls)
	}
}

func TestHookFailureDoesNotBreakInterrupt(t *testing.T) {
	stopHookHome(t)
	recordHook(context.Background(), "agent_loop_stopped", func(context.Context, map[string]any) error { panic(errors.New("plugin exploded")) })
	session := stopHookSession(true)
	interruptSessionTurn(context.Background(), "s1", session, "", false)
	if session.Get("_turn_cancel_requested") != true {
		t.Fatalf("cancel flag %v", session.Get("_turn_cancel_requested"))
	}
}

func TestInterruptHookObserverSeesTheSessionsProfileABA(t *testing.T) {
	launch := stopHookHome(t)
	routed := filepath.Join(launch, "profiles", "s6probe-b")
	if err := os.MkdirAll(routed, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{launch, routed} {
		if err := os.WriteFile(filepath.Join(h, ".env"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var seen []string
	observer := func(ctx context.Context, _ map[string]any) error {
		mu.Lock()
		seen = append(seen, home.Dir(ctx))
		mu.Unlock()
		return nil
	}
	recordHook(context.Background(), "agent_loop_stopped", observer)
	recordHook(home.WithOverride(context.Background(), routed), "agent_loop_stopped", observer)
	provider.SetMultiplexActive(true)
	defer provider.SetMultiplexActive(false)
	for _, profileHome := range []any{nil, routed, nil} {
		session := stopHookSession(true)
		session.Set("profile_home", profileHome)
		interruptSessionTurn(context.Background(), "s1", session, "", false)
		if got := home.Dir(context.Background()); got != launch {
			t.Fatalf("home binding leaked past the dispatch %s", got)
		}
	}
	if !reflect.DeepEqual(seen, []string{launch, routed, launch}) {
		t.Fatalf("observer saw %v", seen)
	}
}
