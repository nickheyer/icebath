package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
)

// Parent agent stand in exposing only the attributes the hermes fake sets
type fakeBackgroundParent struct {
	model    string
	provider string
	disabled []string
	chain    []map[string]any
}

func (f fakeBackgroundParent) BaseURL() string                        { return "" }
func (f fakeBackgroundParent) APIKey() string                         { return "" }
func (f fakeBackgroundParent) Provider() string                       { return f.provider }
func (f fakeBackgroundParent) APIMode() string                        { return "" }
func (f fakeBackgroundParent) ACPCommand() string                     { return "" }
func (f fakeBackgroundParent) ACPArgs() []string                      { return nil }
func (f fakeBackgroundParent) EphemeralSystemPrompt() string          { return "" }
func (f fakeBackgroundParent) ProviderRouting() agent.ProviderRouting { return agent.ProviderRouting{} }
func (f fakeBackgroundParent) Model() string                          { return f.model }
func (f fakeBackgroundParent) EnabledToolsets() []string              { return nil }
func (f fakeBackgroundParent) DisabledToolsets() []string             { return f.disabled }
func (f fakeBackgroundParent) ReasoningConfig() map[string]any        { return nil }
func (f fakeBackgroundParent) ServiceTier() string                    { return "" }
func (f fakeBackgroundParent) RequestOverrides() map[string]any       { return map[string]any{} }
func (f fakeBackgroundParent) SessionStore() agent.SessionStore       { return nil }
func (f fakeBackgroundParent) FallbackChain() []map[string]any        { return f.chain }

// Home whose config.yaml holds the given text, the profile the background kwargs read
func backgroundCfgHome(t *testing.T, cfg string) context.Context {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	useSharedDB(t, nil)
	return context.Background()
}

func TestBackgroundAgentKwargsPreservesFullFallbackChain(t *testing.T) {
	ctx := backgroundCfgHome(t, "max_turns: 25\n")
	chain := []map[string]any{{"provider": "openrouter", "model": "openai/gpt-5.5"}, {"provider": "anthropic", "model": "claude-sonnet-4-6"}}
	parent := fakeBackgroundParent{model: "gpt-5.5", provider: "openai-codex", chain: chain}
	if got := backgroundAgentKwargs(ctx, parent, "task-id").FallbackModel; !reflect.DeepEqual(got, chain) {
		t.Fatalf("fallback_model %v", got)
	}
}

func TestBackgroundAgentKwargsPreservesEmptyFallbackChain(t *testing.T) {
	ctx := backgroundCfgHome(t, "max_turns: 25\nfallback_providers:\n  - provider: openrouter\n    model: openai/gpt-5.5\n")
	parent := fakeBackgroundParent{model: "gpt-5.5", provider: "anthropic", chain: []map[string]any{}}
	if got := backgroundAgentKwargs(ctx, parent, "task-id").FallbackModel; got == nil || len(got) != 0 {
		t.Fatalf("fallback_model %v", got)
	}
}

func TestBackgroundAgentKwargsForwardsAgentDisabledToolsets(t *testing.T) {
	ctx := backgroundCfgHome(t, "max_turns: 25\n")
	parent := fakeBackgroundParent{model: "gpt-5.5", provider: "anthropic", chain: []map[string]any{}, disabled: []string{"browser"}}
	if got := backgroundAgentKwargs(ctx, parent, "task-id").DisabledToolsets; !reflect.DeepEqual(got, []string{"browser"}) {
		t.Fatalf("disabled_toolsets %v", got)
	}
}

func TestBackgroundAgentKwargsFallsBackToConfigDisabledToolsets(t *testing.T) {
	ctx := backgroundCfgHome(t, "max_turns: 25\n")
	saved := loadDisabledToolsets
	loadDisabledToolsets = func(context.Context) []string { return []string{"browser"} }
	t.Cleanup(func() { loadDisabledToolsets = saved })
	parent := fakeBackgroundParent{model: "gpt-5.5", provider: "anthropic", chain: []map[string]any{}}
	if got := backgroundAgentKwargs(ctx, parent, "task-id").DisabledToolsets; !reflect.DeepEqual(got, []string{"browser"}) {
		t.Fatalf("disabled_toolsets %v", got)
	}
}

func TestBackgroundAgentKwargsReadsNestedMaxTurns(t *testing.T) {
	ctx := backgroundCfgHome(t, "agent:\n  max_turns: 300\n")
	parent := fakeBackgroundParent{model: "test-model"}
	if got := backgroundAgentKwargs(ctx, parent, "task_1").MaxIterations; got == nil || *got != 300 {
		t.Fatalf("max_iterations %v", got)
	}
}

func TestBackgroundAgentKwargsFallsBackToRootMaxTurns(t *testing.T) {
	ctx := backgroundCfgHome(t, "max_turns: 50\n")
	parent := fakeBackgroundParent{model: "test-model"}
	if got := backgroundAgentKwargs(ctx, parent, "task_1").MaxIterations; got == nil || *got != 50 {
		t.Fatalf("max_iterations %v", got)
	}
}

func TestBackgroundAgentKwargsHandlesNullAgentConfig(t *testing.T) {
	ctx := backgroundCfgHome(t, "agent: null\nmax_turns: 40\n")
	parent := fakeBackgroundParent{model: "test-model"}
	if got := backgroundAgentKwargs(ctx, parent, "task_1").MaxIterations; got == nil || *got != 40 {
		t.Fatalf("max_iterations %v", got)
	}
}

// Parent stand in holding a dedicated store, the hermes SimpleNamespace with _session_db
type fakeStoreParent struct {
	fakeBackgroundParent
	store agent.SessionStore
}

func (f fakeStoreParent) SessionStore() agent.SessionStore { return f.store }

func TestBackgroundSideAgentPersistsIntoTheParentAgentStore(t *testing.T) {
	ctx := backgroundCfgHome(t, "max_turns: 25\n")
	parentDB, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parentDB.Close() })
	parent := fakeStoreParent{fakeBackgroundParent{model: "m", provider: "p", chain: []map[string]any{}}, parentDB}
	if got := backgroundAgentKwargs(ctx, parent, "bg_1").SessionDB; got != agent.SessionStore(parentDB) {
		t.Fatalf("session_db %v, want the parent's store", got)
	}
}

func TestBackgroundSideAgentHoldsItsOwnRegistryReference(t *testing.T) {
	ctx := backgroundCfgHome(t, "max_turns: 25\n")
	profileHome := filepath.Join(t.TempDir(), "profiles", "work")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	baseline := state.RegistryStats()["live_generations"]
	parentDB, err := state.Acquire(ctx, filepath.Join(profileHome, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	side, release, err := sideAgentSessionDB(ctx, parentDB)
	if err != nil {
		t.Fatal(err)
	}
	sideDB, isDB := side.(*state.DB)
	if !isDB || sideDB.Path() != parentDB.Path() {
		t.Fatalf("side store %v", side)
	}
	state.ReleaseOrClose(parentDB)
	if live := state.RegistryStats()["live_generations"]; live != baseline+1 {
		t.Fatalf("live generations %d after the parent released, want %d", live, baseline+1)
	}
	if err := sideDB.CreateSession(state.NewSession{ID: "bg_1", Source: "tui", Model: "m"}); err != nil {
		t.Fatalf("side store closed under the turn: %v", err)
	}
	release()
	if live := state.RegistryStats()["live_generations"]; live != baseline {
		t.Fatalf("live generations %d after the side agent released, want %d", live, baseline)
	}
}
