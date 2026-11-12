package tuiserver

import (
	"context"
	"maps"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Enabled keys of the cli platform the runtime resolves from cfg without default MCP servers
func profilesPinRuntime(ctx context.Context, cfg config.Tree) map[string]bool {
	out := map[string]bool{}
	for key, on := range tools.Default.PlatformTools(ctx, tools.ScopeKey(home.Dir(ctx)), cfg, "cli", tools.PlatformToolsOptions{}) {
		if on {
			out[key] = true
		}
	}
	return out
}

// Clears the multi profile hosting latch for the case and puts the prior state back, as the hermes conftest does per test
func resetHostingLatch(t *testing.T) {
	t.Helper()
	prev := provider.IsMultiplexActive(context.Background())
	provider.SetMultiplexActive(false)
	servedProfileMu.Lock()
	prevServed := servedProfileHomes
	servedProfileHomes = map[string]bool{}
	servedProfileMu.Unlock()
	t.Cleanup(func() {
		provider.SetMultiplexActive(prev)
		servedProfileMu.Lock()
		servedProfileHomes = prevServed
		servedProfileMu.Unlock()
	})
}

// Pins names then answers what the runtime resolves and what the profile editor reports pinned
func profilesPinAndResolve(t *testing.T, ctx context.Context, cfg config.Tree, names []any) (map[string]bool, map[string]bool) {
	t.Helper()
	if err := saveToolsetPin(ctx, home.Dir(ctx), cfg, names); err != nil {
		t.Fatal(err)
	}
	_, pinned := describeToolsets(ctx, cfg)
	return profilesPinRuntime(ctx, cfg), pinned
}

func TestToolsetPinIsWhatTheRuntimeReads(t *testing.T) {
	resetHostingLatch(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	cfg := config.Tree{}
	resolved, pinned := profilesPinAndResolve(t, ctx, cfg, []any{"web"})
	web := map[string]bool{"web": true}
	if !maps.Equal(resolved, web) || !maps.Equal(pinned, web) {
		t.Fatalf("resolved %v pinned %v", resolved, pinned)
	}
	if section, _ := cfg["tools"].(map[string]any); section != nil {
		if _, phantom := section["enabled_toolsets"]; phantom {
			t.Fatal("the writer left the phantom key behind")
		}
	}
}

func TestToolsetPinRoundTripsAThenBThenA(t *testing.T) {
	for _, second := range [][]any{{"file", "terminal"}, {}} {
		t.Run("", func(t *testing.T) {
			resetHostingLatch(t)
			t.Setenv("ICEBATH_HOME", t.TempDir())
			ctx := context.Background()
			cfg := config.Tree{}
			first, _ := profilesPinAndResolve(t, ctx, cfg, []any{"web"})
			profilesPinAndResolve(t, ctx, cfg, second)
			again, pinned := profilesPinAndResolve(t, ctx, cfg, []any{"web"})
			web := map[string]bool{"web": true}
			if !maps.Equal(first, web) || !maps.Equal(again, web) || !maps.Equal(pinned, web) {
				t.Fatalf("second %v first %v again %v pinned %v", second, first, again, pinned)
			}
		})
	}
}

func TestEmptyPinClearsThePinAndFallsBackToThePlatformDefault(t *testing.T) {
	resetHostingLatch(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	cfg := config.Tree{}
	profilesPinAndResolve(t, ctx, cfg, []any{"web"})
	if err := saveToolsetPin(ctx, home.Dir(ctx), cfg, []any{}); err != nil {
		t.Fatal(err)
	}
	if _, pinned := describeToolsets(ctx, cfg); pinned != nil {
		t.Fatalf("pinned %v", pinned)
	}
	if resolved := profilesPinRuntime(ctx, cfg); maps.Equal(resolved, map[string]bool{"web": true}) {
		t.Fatal("the cleared pin still resolves to web only")
	}
}
