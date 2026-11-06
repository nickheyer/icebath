package tools

import (
	"context"
	"slices"
	"testing"
)

func withPluginCatalog(t *testing.T, toolsets []ConfigurableToolset, projected []ConfigurableToolset, isProjected bool) *int {
	t.Helper()
	saved, savedGateway := PluginToolsetCatalog, GatewayPluginToolsets
	t.Cleanup(func() { PluginToolsetCatalog, GatewayPluginToolsets = saved, savedGateway })
	discovered := 0
	PluginToolsetCatalog.Discover = func(context.Context) { discovered++ }
	PluginToolsetCatalog.Toolsets = func(context.Context) []ConfigurableToolset { return toolsets }
	PluginToolsetCatalog.KeysNowait = func(context.Context) map[string]bool {
		out := map[string]bool{}
		for _, ts := range toolsets {
			out[ts.Key] = true
		}
		return out
	}
	GatewayPluginToolsets = func() ([]ConfigurableToolset, map[string]bool, bool) {
		return projected, map[string]bool{}, isProjected
	}
	return &discovered
}

func TestToolsConfigEffectiveConfigurableToolsetsDedupesBundledPlugins(t *testing.T) {
	discovered := withPluginCatalog(t, []ConfigurableToolset{{Key: "web", Label: "dup"}, {Key: "dplat", Label: "🔌 Dplat"}, {Key: "dplat", Label: "again"}}, nil, false)
	all := EffectiveConfigurableToolsets(context.Background())
	seen := map[string]bool{}
	for _, ts := range all {
		if seen[ts.Key] {
			t.Fatalf("duplicate key %s", ts.Key)
		}
		seen[ts.Key] = true
	}
	if *discovered != 1 || all[len(all)-1] != (ConfigurableToolset{Key: "dplat", Label: "🔌 Dplat"}) || len(all) != len(ConfigurableToolsets)+1 {
		t.Fatalf("%d %v", *discovered, all[len(all)-1])
	}
	if got := toolsetLabel(context.Background(), "dplat"); got != "🔌 Dplat" {
		t.Fatal(got)
	}
	if got := toolsetLabel(context.Background(), "nope"); got != "nope" {
		t.Fatal(got)
	}
}

func TestToolsConfigProjectedPluginToolsetsSkipDiscovery(t *testing.T) {
	discovered := withPluginCatalog(t, []ConfigurableToolset{{Key: "local"}}, []ConfigurableToolset{{Key: "gw", Label: "Gateway"}}, true)
	all := EffectiveConfigurableToolsets(context.Background())
	if *discovered != 0 || all[len(all)-1].Key != "gw" {
		t.Fatalf("%d %v", *discovered, all[len(all)-1])
	}
	if keys := PluginToolsetKeys(context.Background()); len(keys) != 1 || !keys["gw"] {
		t.Fatal(keys)
	}
	GatewayPluginToolsets = nil
	if keys := PluginToolsetKeys(context.Background()); len(keys) != 1 || !keys["local"] {
		t.Fatal(keys)
	}
	if !checklistToolsetKeys(context.Background(), "cli")["local"] || checklistToolsetKeys(context.Background(), "cli")["stt"] {
		t.Fatal("checklist keys")
	}
}

func TestToolsConfigPluginFailureFallsBackToBuiltIns(t *testing.T) {
	withPluginCatalog(t, nil, nil, false)
	PluginToolsetCatalog.KeysNowait = func(context.Context) map[string]bool { panic("plugin import failed") }
	PluginToolsetCatalog.Toolsets = func(context.Context) []ConfigurableToolset { panic("plugin import failed") }
	if keys := PluginToolsetKeys(context.Background()); keys == nil || len(keys) != 0 {
		t.Fatal(keys)
	}
	if got := EffectiveConfigurableToolsets(context.Background()); !slices.Equal(got, ConfigurableToolsets) {
		t.Fatal(got)
	}
}

func TestToolsConfigGUIToolsetLabelAndConfigurationPlatform(t *testing.T) {
	for in, want := range map[string]string{"🔌 My Plugin ": "My Plugin", "Web Search": "Web Search", "  x": "x", "": "", "🔍　Search": "Search", "a🔌 Tool": "a🔌 Tool"} {
		if got := GUIToolsetLabel(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	if got := ToolsetConfigurationPlatform("discord", "cli"); got != "discord" {
		t.Fatal(got)
	}
	if got := ToolsetConfigurationPlatform("web", "cli"); got != "cli" {
		t.Fatal(got)
	}
	if got := ToolsetConfigurationPlatform("discord", "discord"); got != "discord" {
		t.Fatal(got)
	}
}

func TestToolsConfigCategoriesListEveryProviderEnvKey(t *testing.T) {
	keys := ToolProviderEnvKeys()
	for _, want := range []string{"VOICE_TOOLS_OPENAI_KEY", "ELEVENLABS_API_KEY", "MISTRAL_API_KEY", "GEMINI_API_KEY", "DEEPINFRA_API_KEY", "GROQ_API_KEY", "FIRECRAWL_API_URL", "XAI_API_KEY"} {
		if !slices.Contains(keys, want) {
			t.Errorf("missing %s", want)
		}
	}
	if got := ToolCategories.Keys; !slices.Equal(got, []string{"tts", "stt", "web", "image_gen", "video_gen", "x_search", "browser", "computer_use"}) {
		t.Fatal(got)
	}
	if !isConfigurableToolset("tts") || !isConfigurableToolset("vision") || isConfigurableToolset("terminal") {
		t.Fatal("configurable")
	}
	tts, _ := ToolCategories.Get("tts")
	providers, _ := tts.(interface{ Get(string) (any, bool) }).Get("providers")
	row := providers.([]any)[1].(interface {
		Get(string) (any, bool)
	})
	if v, _ := row.Get("requires_nous_auth"); v != true {
		t.Fatal("nous marker")
	}
}
