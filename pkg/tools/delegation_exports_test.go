package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func orderDef(name, toolset string) Def {
	return Def{Name: name, Toolset: toolset, Schema: map[string]any{"name": name, "parameters": map[string]any{"type": "object"}},
		Handler: func(context.Context, *Call) string { return "{}" }}
}

func TestOrderedToolsetNamesStaticOrderThenPlugins(t *testing.T) {
	r := NewRegistry()
	r.Register(orderDef("zz_plugin_tool", "zz_plugin"))
	r.Register(orderDef("aa_plugin_tool", "aa_plugin"))
	r.Register(orderDef("mcp_tool", "mcp-docs"))
	r.RegisterToolsetAlias("docs", "mcp-docs")
	r.RegisterToolsetAlias("web", "mcp-docs")
	got := r.OrderedToolsetNames(context.Background(), "")
	want := append(orderedToolsetNames(), "aa_plugin", "docs", "zz_plugin")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order\n got %v\nwant %v", got, want)
	}
	all := r.AllToolsets(context.Background(), "")
	if len(all) != len(got) {
		t.Fatalf("all toolsets has %d keys, ordered names %d", len(all), len(got))
	}
	for _, n := range got {
		if _, ok := all[n]; !ok {
			t.Fatalf("%s missing from AllToolsets", n)
		}
	}
}

func TestOrderedToolsetNamesCustomToolsetAppended(t *testing.T) {
	CreateCustomToolset("zz_custom_order_probe", "probe", []string{"terminal"}, nil)
	t.Cleanup(func() {
		toolsetsMu.Lock()
		delete(Toolsets, "zz_custom_order_probe")
		toolsetOrder = toolsetOrder[:len(toolsetOrder)-1]
		toolsetsMu.Unlock()
	})
	got := NewRegistry().OrderedToolsetNames(context.Background(), "")
	if got[len(got)-1] != "zz_custom_order_probe" || got[0] != "web" {
		t.Fatalf("custom toolset not appended after the static table: %v", got)
	}
}

func TestAvailableToolsetNamesFirstRegistrationOrder(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry()
	r.Register(orderDef("t1", "beta"))
	r.Register(orderDef("t2", "alpha"))
	r.Register(orderDef("t3", "beta"))
	if got := r.AvailableToolsetNames(ctx, nil); !reflect.DeepEqual(got, []string{"beta", "alpha"}) {
		t.Fatalf("got %v", got)
	}
	r.Register(orderDef("t1", "beta"))
	if got := r.AvailableToolsetNames(ctx, nil); !reflect.DeepEqual(got, []string{"beta", "alpha"}) {
		t.Fatalf("replacement moved the key: %v", got)
	}
	r.Remove("t1")
	r.Remove("t3")
	r.Register(orderDef("t1", "beta"))
	if got := r.AvailableToolsetNames(ctx, nil); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("re-registration after removal must append: %v", got)
	}
	names := map[string]bool{}
	for n := range r.AvailableToolsets(ctx, nil) {
		names[n] = true
	}
	if len(names) != 2 || !names["alpha"] || !names["beta"] {
		t.Fatalf("AvailableToolsets keys %v", names)
	}
}

func TestAvailableToolsetNamesScopedOverlayAppendsInRegistrationOrder(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry()
	r.Register(orderDef("g1", "global_ts"))
	profile := t.TempDir()
	env := &Env{Home: profile}
	if err := r.RegisterScoped(profile, orderDef("p_zz", "zeta_ts")); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterScoped(profile, orderDef("p_aa", "alpha_ts")); err != nil {
		t.Fatal(err)
	}
	if got := r.AvailableToolsetNames(ctx, env); !reflect.DeepEqual(got, []string{"global_ts", "zeta_ts", "alpha_ts"}) {
		t.Fatalf("got %v", got)
	}
	if got := r.AvailableToolsetNames(ctx, nil); !reflect.DeepEqual(got, []string{"global_ts"}) {
		t.Fatalf("global view %v", got)
	}
}

func TestSessionPlatformToolDropsDefaultRegistry(t *testing.T) {
	for _, platform := range []string{"cli", "tui", "telegram", "cron", ""} {
		if !SessionPlatformToolDrops(platform)["manage_catalog"] {
			t.Fatalf("manage_catalog kept on %q", platform)
		}
	}
	if SessionPlatformToolDrops("desktop")["manage_catalog"] {
		t.Fatal("manage_catalog dropped on desktop")
	}
}

func TestResolveActiveProfileName(t *testing.T) {
	ctx := context.Background()
	t.Run("default when home is root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), ".icebath")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_HOME", root)
		if got := ResolveActiveProfileName(ctx); got != "default" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("named profile under the root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), ".icebath")
		profile := filepath.Join(root, "profiles", "coder")
		if err := os.MkdirAll(profile, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_HOME", profile)
		if got := ResolveActiveProfileName(ctx); got != "coder" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("falls back to default on resolution failure", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "bad\xed\xa0\x80"))
		if got := ResolveActiveProfileName(ctx); got != "default" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestToolSearchScopedNamesFollowsHostSelection(t *testing.T) {
	ctx := context.Background()
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	all := ToolSearchScopedNames(ctx, host)
	if got := NewExecutor(host, nil).toolSearchScopedNames(ctx); !reflect.DeepEqual(got, all) {
		t.Fatalf("executor form %v differs from exported form %v", got, all)
	}
	for name := range all {
		ts := Default.ToolsetForTool(ctx, "", name)
		if ts == "" {
			continue
		}
		host.disabled = []string{ts}
		if ToolSearchScopedNames(ctx, host)[name] {
			t.Fatalf("%s still scoped with toolset %s disabled", name, ts)
		}
		return
	}
}
