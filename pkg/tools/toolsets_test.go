package tools

import (
	"context"
	"reflect"
	"testing"
)

func TestResolveToolsetBundlesAndIncludes(t *testing.T) {
	r := NewRegistry()
	ctx := context.Background()
	cli := r.ResolveToolset(ctx, "", "icebath-cli")
	if len(cli) != len(CoreTools) {
		t.Fatalf("icebath-cli resolved %d tools, want %d", len(cli), len(CoreTools))
	}
	legacy := r.ResolveToolset(ctx, "", "hermes-cli")
	if len(legacy) != len(cli) {
		t.Fatalf("legacy hermes-cli spelling resolved %d tools, want %d", len(legacy), len(cli))
	}
	debugging := r.ResolveToolset(ctx, "", "debugging")
	for _, want := range []string{"terminal", "process_manage", "web_search", "web_extract", "read_file", "patch"} {
		if !containsString(debugging, want) {
			t.Errorf("debugging missing %s", want)
		}
	}
	if !r.ValidateToolset(ctx, "", "all") || r.ValidateToolset(ctx, "", "nope") {
		t.Fatal("validate wildcard or unknown toolset wrong")
	}
	if containsString(r.ResolveToolset(ctx, "", "all"), "setup_beat") {
		t.Fatal("platform gated toolset leaked into the all expansion")
	}
}

func TestSelectToolNamesSubtractsDisabledLast(t *testing.T) {
	r := NewRegistry()
	ctx := context.Background()
	tools, _ := r.SelectToolNames(ctx, nil, []string{"icebath-cli"}, []string{"browser"}, SelectionContext{})
	if tools["browser_exec"] || !tools["web_search"] || !tools["terminal"] {
		t.Fatalf("disabled browser toolset handling wrong: %v", tools)
	}
	tools, _ = r.SelectToolNames(ctx, nil, []string{"icebath-cli"}, []string{"icebath-discord"}, SelectionContext{})
	if !tools["terminal"] || tools["discord"] {
		t.Fatalf("disabling a bundle must strip only its non core delta: %v", tools)
	}
}

func TestScopedRegistrationOverlay(t *testing.T) {
	r := NewRegistry()
	ctx := context.Background()
	r.Register(Def{Name: "alpha", Toolset: "web", Schema: map[string]any{"name": "alpha"}, Handler: func(context.Context, *Call) string { return "" }})
	err := r.RegisterScoped("/tmp/profile-a", Def{Name: "alpha", Toolset: "mcp-x", Plugin: "icebath_plugins.p", Schema: map[string]any{}, Handler: func(context.Context, *Call) string { return "" }})
	if err == nil {
		t.Fatal("cross toolset shadow without override must be rejected")
	}
	if err := r.RegisterScoped("/tmp/profile-a", Def{Name: "beta", Toolset: "mcp-x", Schema: map[string]any{}, Handler: func(context.Context, *Call) string { return "" }}); err != nil {
		t.Fatal(err)
	}
	if r.Lookup(ctx, ScopeKey("/tmp/profile-a"), "beta") == nil || r.Lookup(ctx, "", "beta") != nil {
		t.Fatal("scoped overlay visibility wrong")
	}
	if !containsString(r.ToolNamesForToolset(ctx, ScopeKey("/tmp/profile-a"), "mcp-x"), "beta") {
		t.Fatal("scoped toolset listing missing beta")
	}
}

func TestParsePlatformToolsetsValue(t *testing.T) {
	got, ok := ParsePlatformToolsetsValue(`["browser", 'terminal']`)
	if !ok || len(got) != 2 || got[0] != "browser" || got[1] != "terminal" {
		t.Fatalf("list literal parse wrong: %v %v", got, ok)
	}
	if _, ok := ParsePlatformToolsetsValue("browser"); ok {
		t.Fatal("scalar string must not parse as a list")
	}
}

func TestConfigurableKeysExportsEveryConfigurableToolsetKey(t *testing.T) {
	got := ConfigurableKeys()
	if len(got) != len(configurableKeys()) || len(got) == 0 {
		t.Fatalf("got %d keys want %d", len(got), len(configurableKeys()))
	}
	for _, c := range ConfigurableToolsets {
		if !got[c.Key] {
			t.Fatalf("missing %q", c.Key)
		}
	}
}

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

func TestStaticToolsetWrappersMatchTheTable(t *testing.T) {
	names := OrderedStaticToolsetNames()
	if !reflect.DeepEqual(names, orderedToolsetNames()) || len(names) == 0 {
		t.Fatalf("ordered names = %v", names)
	}
	spec, ok := StaticToolset("web")
	want, _ := staticToolset("web")
	if !ok || !reflect.DeepEqual(spec, want) {
		t.Fatalf("StaticToolset(web) = %v %v", spec, ok)
	}
	if _, ok := StaticToolset("no-such-toolset"); ok {
		t.Fatal("unknown toolset resolved")
	}
}
