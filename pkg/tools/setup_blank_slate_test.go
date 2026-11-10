package tools

import (
	"context"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestBlankSlateListExcludesSearchButKeepsKanban(t *testing.T) {
	cfg := config.Tree{}
	if err := BlankSlateMinimalToolsets(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var disabled []string
	for _, v := range cfg["agent"].(map[string]any)["disabled_toolsets"].([]any) {
		disabled = append(disabled, v.(string))
	}
	if slices.Contains(disabled, "search") {
		t.Fatalf("search disabled %v", disabled)
	}
	if !slices.Contains(disabled, "kanban") || !slices.Contains(disabled, "web") {
		t.Fatalf("kanban or web kept %v", disabled)
	}
	for _, kept := range []string{"file", "terminal", "vision", "skills"} {
		if slices.Contains(disabled, kept) {
			t.Fatalf("%s disabled %v", kept, disabled)
		}
	}
	if !slices.IsSorted(disabled) {
		t.Fatalf("unsorted %v", disabled)
	}
	cli := cfg["platform_toolsets"].(map[string]any)["cli"]
	if !slices.Equal(config.AsStrings(cli), []string{"file", "skills", "terminal", "vision"}) {
		t.Fatalf("cli %v", cli)
	}
}

func TestBlankSlateMinimalToolsetsOddSections(t *testing.T) {
	if err := BlankSlateMinimalToolsets(context.Background(), config.Tree{"platform_toolsets": []any{}}); err == nil || err.Error() != "TypeError: 'list' object does not support item assignment" {
		t.Fatalf("%v", err)
	}
	cfg := config.Tree{"agent": nil}
	if err := BlankSlateMinimalToolsets(context.Background(), cfg); err != nil || cfg["agent"] != nil {
		t.Fatalf("agent %v %v", cfg["agent"], err)
	}
	cfg = config.Tree{"agent": map[string]any{"max_turns": 9}, "platform_toolsets": map[string]any{"telegram": []any{"web"}}}
	if err := BlankSlateMinimalToolsets(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["agent"].(map[string]any)["max_turns"] != 9 || cfg["platform_toolsets"].(map[string]any)["telegram"] == nil {
		t.Fatalf("existing sections dropped %v", cfg)
	}
}

func TestBlankSlateMinimalToolsetsNoDisabledBundleOverlapsKeptTools(t *testing.T) {
	ctx := context.Background()
	cfg := config.Tree{}
	if err := BlankSlateMinimalToolsets(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, ts := range config.AsStrings(cfg["platform_toolsets"].(map[string]any)["cli"]) {
		for _, tool := range Default.ResolveToolset(ctx, "", ts) {
			kept[tool] = true
		}
	}
	for _, ts := range config.AsStrings(cfg["agent"].(map[string]any)["disabled_toolsets"]) {
		for _, tool := range Default.ResolveToolset(ctx, "", ts) {
			if kept[tool] {
				t.Fatalf("disabled toolset '%s' overlaps kept tool %s; it would silently strip it from the blank-slate agent", ts, tool)
			}
		}
	}
}

func TestBlankSlateMinimalToolsetsToolSchemaSurvivesDisabledToolsetsFromConfig(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	vision := Default.Get("vision_analyze")
	if vision == nil {
		t.Fatal("vision_analyze is not registered")
	}
	prev := vision.Check
	vision.Check = func(*Env) bool { return true }
	InvalidateCheckCache()
	ClearToolDefinitionsCache()
	t.Cleanup(func() { vision.Check = prev; InvalidateCheckCache(); ClearToolDefinitionsCache() })
	ctx := context.Background()
	cfg := config.Tree{}
	if err := BlankSlateMinimalToolsets(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := BlankSlateMinimizeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	enabled := sortedSet(platformTools(cfg, "cli", true))
	disabled := ParseConfigStringList(cfg.Get("agent.disabled_toolsets"))
	legacy := config.Tree{"tools": map[string]any{"tool_search": map[string]any{"enabled": "on", "defer": []any{}}}}
	names := map[string]bool{}
	for _, td := range Default.ToolDefinitions(ctx, &Env{Home: h, Config: legacy}, DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: true}) {
		names[toolDefName(td)] = true
	}
	for _, n := range []string{"terminal", "read_file", "write_file", "patch", "search_files"} {
		if !names[n] {
			t.Fatalf("%s missing from %v", n, names)
		}
	}
}

func TestBlankSlateMinimizeConfigOptionalFeaturesTurnedOff(t *testing.T) {
	cfg := config.Tree{}
	if err := BlankSlateMinimizeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{{"compression", "enabled"}, {"memory", "memory_enabled"}, {"memory", "user_profile_enabled"},
		{"checkpoints", "enabled"}, {"smart_model_routing", "enabled"}} {
		if v, ok := config.CfgGet(cfg, path...); !ok || v != false {
			t.Fatalf("%v is %v", path, v)
		}
	}
	if v, _ := config.CfgGet(cfg, "agent", "max_turns"); v != 90 {
		t.Fatalf("max_turns %v", v)
	}
	bad := config.Tree{"agent": map[string]any{}, "memory": "on"}
	if err := BlankSlateMinimizeConfig(bad); err == nil || err.Error() != "TypeError: 'str' object does not support item assignment" {
		t.Fatalf("non mapping memory section %v", err)
	}
	if v, _ := config.CfgGet(bad, "compression", "enabled"); v != false {
		t.Fatal("sections before the failing one keep their writes")
	}
}
