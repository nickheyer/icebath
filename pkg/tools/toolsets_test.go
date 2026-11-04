package tools

import (
	"context"
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
