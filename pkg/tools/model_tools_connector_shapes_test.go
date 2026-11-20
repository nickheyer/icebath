package tools

import (
	"context"
	"sync/atomic"
	"testing"
)

// One registry with a fake connector toolset whose portal gate the test flips
func fakeConnectorRegistry(t *testing.T) (*Registry, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	r := NewRegistry()
	var open atomic.Bool
	var ran atomic.Int32
	gate := func(*Env) bool { return open.Load() }
	d := registerDef("fake_connections", "fakeconn", func(context.Context, *Call) string {
		ran.Add(1)
		return `{"ok": true}`
	})
	d.Check = gate
	if err := r.RegisterScoped("", d); err != nil {
		t.Fatal(err)
	}
	InvalidateCheckCache()
	ClearToolDefinitionsCache()
	t.Cleanup(func() {
		InvalidateCheckCache()
		ClearToolDefinitionsCache()
	})
	return r, &open, &ran
}

// Names a fresh session build receives with the tool_search step skipped, as _session_tool_names asserts
func fakeSessionNames(r *Registry, env *Env, enabled, disabled []string) map[string]bool {
	InvalidateCheckCache()
	names := map[string]bool{}
	for _, td := range r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: true, SkipToolSearchAssembly: true}) {
		names[toolDefName(td)] = true
	}
	InvalidateCheckCache()
	return names
}

// test_cli_session_gets_the_tool_outside_a_code_workspace and the portal gate case over a fake connector toolset
func TestFakeConnectorToolsetFollowsSelectionAndGate(t *testing.T) {
	r, open, _ := fakeConnectorRegistry(t)
	env := &Env{Home: t.TempDir()}
	open.Store(true)
	if !fakeSessionNames(r, env, []string{"fakeconn"}, nil)["fake_connections"] {
		t.Fatal("selection naming the toolset lost its tool with the gate open")
	}
	open.Store(false)
	if fakeSessionNames(r, env, []string{"fakeconn"}, nil)["fake_connections"] {
		t.Fatal("closed gate still reached the schema")
	}
	if fakeSessionNames(r, env, []string{}, nil)["fake_connections"] || fakeSessionNames(r, env, nil, nil)["fake_connections"] {
		t.Fatal("closed gate reached the schema without a selection")
	}
	open.Store(true)
	if !fakeSessionNames(r, env, nil, nil)["fake_connections"] {
		t.Fatal("no selection lost the registered toolset with the gate open")
	}
}

// test_operator_can_still_turn_it_off over a fake connector toolset
func TestFakeConnectorToolsetDisabledByNameNotByComposite(t *testing.T) {
	r, open, _ := fakeConnectorRegistry(t)
	env := &Env{Home: t.TempDir()}
	open.Store(true)
	if fakeSessionNames(r, env, []string{"fakeconn"}, []string{"fakeconn"})["fake_connections"] {
		t.Fatal("disabling the toolset by name kept its tool")
	}
	if !fakeSessionNames(r, env, []string{"fakeconn"}, []string{"icebath-cli"})["fake_connections"] {
		t.Fatal("disabling a platform composite stripped the toolset")
	}
}

// The check verdict cache the connector cases invalidate around every build, and the no cache opt out
func TestFakeConnectorGateVerdictCache(t *testing.T) {
	_, open, _ := fakeConnectorRegistry(t)
	env := &Env{Home: t.TempDir()}
	var probes atomic.Int32
	gate := func(*Env) bool { probes.Add(1); return open.Load() }
	open.Store(true)
	if !Default.checkCached(context.Background(), env, gate) {
		t.Fatal("first probe")
	}
	open.Store(false)
	if !Default.checkCached(context.Background(), env, gate) || probes.Load() != 1 {
		t.Fatalf("verdict not reused within its window, probes %d", probes.Load())
	}
	if got, ok := CachedCheckResult(env, gate); !ok || !got {
		t.Fatalf("cached verdict %v %v", got, ok)
	}
	InvalidateCheckCache()
	if _, ok := CachedCheckResult(env, gate); ok {
		t.Fatal("invalidation kept the verdict")
	}
	if Default.checkCached(context.Background(), env, gate) || probes.Load() != 2 {
		t.Fatalf("flip not seen after invalidation, probes %d", probes.Load())
	}
	live := NoCacheCheck(gate)
	open.Store(true)
	if !Default.checkCached(context.Background(), env, live) {
		t.Fatal("no cache check read a stale verdict")
	}
	open.Store(false)
	if Default.checkCached(context.Background(), env, live) {
		t.Fatal("no cache check cached its verdict")
	}
}

// test_session_the_portal_has_not_enabled_never_receives_the_tool direct path, registry dispatch never consults check_fn
func TestFakeConnectorDispatchRunsTheHandlerWhateverTheGate(t *testing.T) {
	r, open, ran := fakeConnectorRegistry(t)
	open.Store(false)
	d := &Dispatcher{Registry: r}
	if got := d.RegistryDispatch(context.Background(), &Call{Name: "fake_connections", Args: map[string]any{"action": "status"}, Env: &Env{Home: t.TempDir()}}); got != `{"ok": true}` || ran.Load() != 1 {
		t.Fatalf("dispatch %q ran %d", got, ran.Load())
	}
}
