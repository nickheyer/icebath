package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Captures warnings written by the platform toolset resolver
func captureToolsConfigLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	saved := toolsConfigLogger
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	toolsConfigLogger = func() *slog.Logger { return logger }
	t.Cleanup(func() { toolsConfigLogger = saved })
	return buf
}

// Isolates the resolver from plugin catalogs and the xAI auth store
func isolatePlatformResolver(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "")
	savedCatalog, savedXAI := PluginToolsetCatalog, XAIOAuthTokensReader
	PluginToolsetCatalog.KeysNowait = func(context.Context) map[string]bool { return map[string]bool{} }
	PluginToolsetCatalog.Toolsets = func(context.Context) []ConfigurableToolset { return nil }
	PluginToolsetCatalog.PortableNowait = func(context.Context) map[string]bool { return map[string]bool{} }
	PluginToolsetCatalog.Discover = func(context.Context) {}
	XAIOAuthTokensReader = func(context.Context, string) error { return errors.New("not logged in") }
	t.Cleanup(func() { PluginToolsetCatalog, XAIOAuthTokensReader = savedCatalog, savedXAI })
}

func platformTools(cfg config.Tree, platform string, includeMCP bool) map[string]bool {
	return Default.PlatformTools(context.Background(), "", cfg, platform, PlatformToolsOptions{IncludeDefaultMCPServers: includeMCP})
}

// Resolves the cli platform over one platform_toolsets list and reports whether the #38798 warning fired
func warnsAllInvalid(t *testing.T, buf *bytes.Buffer, list []any) bool {
	t.Helper()
	buf.Reset()
	discardPlatformToolsetsWarning("cli")
	platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": list}}, "cli", true)
	return strings.Contains(buf.String(), "#38798")
}

func TestPlatformToolsetWarnings(t *testing.T) {
	isolatePlatformResolver(t)
	buf := captureToolsConfigLog(t)
	t.Run("test_all_invalid_platform_toolsets_logs_runtime_warning", func(t *testing.T) {
		if !warnsAllInvalid(t, buf, []any{"not-a-real-toolset"}) || !strings.Contains(buf.String(), "not-a-real-toolset") {
			t.Fatalf("all invalid list must warn, got %q", buf.String())
		}
	})
	t.Run("test_valid_platform_toolsets_no_runtime_warning", func(t *testing.T) {
		if warnsAllInvalid(t, buf, []any{"hermes-cli"}) {
			t.Fatalf("valid list must not warn, got %q", buf.String())
		}
	})
	t.Run("test_partially_valid_platform_toolsets_no_runtime_warning", func(t *testing.T) {
		if warnsAllInvalid(t, buf, []any{"hermes-cli", "bogus"}) {
			t.Fatalf("partially valid list must not warn, got %q", buf.String())
		}
	})
	t.Run("test_unloaded_plugin_platform_toolset_no_runtime_warning", func(t *testing.T) {
		unloadedPluginPlatformsDoNotWarn(t, buf)
	})
}

// Bundled and catalog plugin platform entries resolve without the #38798 warning before plugin loading
func unloadedPluginPlatformsDoNotWarn(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	bundled := t.TempDir()
	for _, p := range []string{"teams", "google_chat"} {
		if err := os.MkdirAll(filepath.Join(bundled, "platforms", p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundled, "platforms", p, "plugin.yaml"), []byte("name: "+p+"\nkind: platform\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
	savedBundled := bundledPlatformNames
	bundledPlatformNames = sync.OnceValue(loadBundledPlatformNames)
	t.Cleanup(func() { bundledPlatformNames = savedBundled })
	for _, p := range []string{"teams", "google_chat", "homeassistant"} {
		buf.Reset()
		discardPlatformToolsetsWarning(p)
		platformTools(config.Tree{"platform_toolsets": map[string]any{p: []any{"hermes-" + p}}}, p, true)
		if strings.Contains(buf.String(), "#38798") {
			t.Fatalf("unloaded plugin platform %s must not warn, got %q", p, buf.String())
		}
	}
}

func TestPlatformToolsetsFallBackToPlatformDefault(t *testing.T) {
	isolatePlatformResolver(t)
	buf := captureToolsConfigLog(t)
	def := platformTools(config.Tree{}, "cli", false)
	t.Run("test_null_platform_toolsets_fall_back_to_platform_default", func(t *testing.T) {
		if got := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": nil}}, "cli", false); !maps.Equal(got, def) {
			t.Fatalf("got %v want %v", got, def)
		}
	})
	t.Run("test_scalar_platform_toolsets_fall_back_to_platform_default", func(t *testing.T) {
		if got := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": "bogus"}}, "cli", false); !maps.Equal(got, def) {
			t.Fatalf("got %v want %v", got, def)
		}
	})
	t.Run("test_malformed_list_string_platform_toolsets_warns_then_falls_back", func(t *testing.T) {
		discardPlatformToolsetsWarning("cli")
		buf.Reset()
		got := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": `["web", terminal`}}, "cli", false)
		if !maps.Equal(got, def) {
			t.Fatalf("malformed list string: got %v want %v", got, def)
		}
		if out := buf.String(); !strings.Contains(out, "platform_toolsets.cli") || !strings.Contains(out, "expected a YAML list") {
			t.Fatalf("malformed list string must warn, got %q", out)
		}
	})
}

func TestDiscordToolsetsDoNotLeakAndLegacyAliasExpands(t *testing.T) {
	isolatePlatformResolver(t)
	t.Run("test_discord_toolsets_do_not_leak_to_other_platforms", func(t *testing.T) {
		got := platformTools(config.Tree{"platform_toolsets": map[string]any{"telegram": []any{"hermes-telegram", "discord"}}}, "telegram", true)
		if got["discord"] || got["discord_admin"] {
			t.Fatalf("discord leaked to telegram: %v", got)
		}
	})
	t.Run("test_get_platform_tools_legacy_hermes_alias_expands", func(t *testing.T) {
		enabled := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes", "kanban"}}}, "cli", false)
		ref := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli", "kanban"}}}, "cli", false)
		if len(enabled) == 0 || !maps.Equal(enabled, ref) {
			t.Fatalf("legacy alias with kanban: %v vs %v", enabled, ref)
		}
	})
	t.Run("test_get_platform_tools_legacy_hermes_alias_alone", func(t *testing.T) {
		alone := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes"}}}, "cli", false)
		ref := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli", "hermes-api-server"}}}, "cli", false)
		if len(alone) == 0 || !maps.Equal(alone, ref) {
			t.Fatalf("legacy alias alone: %v vs %v", alone, ref)
		}
	})
}

func TestSavePlatformToolsPreservesMCPServerNames(t *testing.T) {
	isolatePlatformResolver(t)
	cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"web", "terminal", "time", "github", "custom-mcp-server"}}}
	if err := Default.SavePlatformTools(context.Background(), "", cfg, "cli", map[string]bool{"web": true, "browser": true}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	saved := config.AsStrings(cfg.Sub("platform_toolsets")["cli"])
	for _, want := range []string{"time", "github", "custom-mcp-server", "web", "browser"} {
		if !slices.Contains(saved, want) {
			t.Fatalf("%s missing from %v", want, saved)
		}
	}
	if slices.Contains(saved, "terminal") {
		t.Fatalf("terminal must be dropped: %v", saved)
	}
}

func TestPlatformToolsetConsistency(t *testing.T) {
	gateway, ok := staticToolset("icebath-gateway")
	if !ok {
		t.Fatal("icebath-gateway missing")
	}
	t.Run("test_all_platforms_have_toolset_definitions", func(t *testing.T) {
		for _, p := range Platforms {
			if _, ok := staticToolset(p.Info.DefaultToolset); !ok {
				t.Fatalf("platform %q references undefined toolset %q", p.Key, p.Info.DefaultToolset)
			}
		}
	})
	t.Run("test_gateway_toolset_includes_all_messaging_platforms", func(t *testing.T) {
		for _, p := range Platforms {
			if p.Key == "cli" || p.Key == "api_server" || p.Key == "cron" {
				continue
			}
			if !slices.Contains(gateway.Includes, p.Info.DefaultToolset) {
				t.Fatalf("platform %q toolset %q missing from icebath-gateway includes", p.Key, p.Info.DefaultToolset)
			}
		}
	})
}

func TestPlatformToolsMCPServerNames(t *testing.T) {
	isolatePlatformResolver(t)
	t.Run("test_numeric_mcp_server_name_does_not_crash_sorted", func(t *testing.T) {
		cfg := config.Tree{
			"platform_toolsets": map[string]any{"cli": []any{"web", 12306}},
			"mcp_servers":       map[string]any{"12306": map[string]any{"url": "https://example.com/mcp"}, "normal-server": map[string]any{"url": "https://example.com/mcp2"}},
		}
		if got := platformTools(cfg, "cli", true); !got["12306"] {
			t.Fatalf("numeric server name must survive as a string: %v", got)
		}
	})
	t.Run("test_a_saved_disabled_mcp_server_does_not_enable_every_other_server", func(t *testing.T) {
		savedDisabledMCPServerStaysOff(t)
	})
}

// A disabled server in a saved list stays off and enables no other server
func savedDisabledMCPServerStaysOff(t *testing.T) {
	t.Helper()
	for _, listed := range []string{"dead", "mcp-dead"} {
		cfg := config.Tree{
			"platform_toolsets": map[string]any{"cli": []any{"terminal", listed}},
			"mcp_servers": map[string]any{
				"dead": map[string]any{"url": "https://example.com/dead", "enabled": false},
				"live": map[string]any{"url": "https://example.com/live"},
			},
		}
		got := platformTools(cfg, "cli", true)
		if !got["terminal"] || got["dead"] || got["mcp-dead"] || got["live"] {
			t.Fatalf("%s: %v", listed, got)
		}
	}
}

func TestKanbanChecklistReportsAndPersistsExplicitRemoval(t *testing.T) {
	isolatePlatformResolver(t)
	ctx := context.Background()
	cfg := config.Tree{"platform_toolsets": map[string]any{"telegram": []any{"kanban", "web", "terminal"}}}
	current := platformTools(cfg, "telegram", false)
	universe := checklistToolsetKeys(ctx, "telegram")
	next := maps.Clone(current)
	delete(next, "kanban")
	if !current["kanban"] || !universe["kanban"] {
		t.Fatalf("kanban must be enabled and offered: %v %v", current, universe)
	}
	if err := Default.SavePlatformTools(ctx, "", cfg, "telegram", next, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if platformTools(cfg, "telegram", false)["kanban"] {
		t.Fatal("kanban came back after an explicit removal")
	}
	saved := config.AsStrings(cfg.Sub("platform_toolsets")["telegram"])
	if !slices.Contains(saved, "web") || !slices.Contains(saved, "terminal") {
		t.Fatalf("saved list lost entries: %v", saved)
	}
}

// Chooses a configurable toolset inside the cli composite and outside the acp composite as recently shipped
func withRecentlyShipped(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	cli := map[string]bool{}
	for _, n := range Default.ResolveToolset(ctx, "", "icebath-cli") {
		cli[n] = true
	}
	acp := map[string]bool{}
	for _, n := range Default.ResolveToolset(ctx, "", "icebath-acp") {
		acp[n] = true
	}
	for _, c := range ConfigurableToolsets {
		if DefaultOffToolsets[c.Key] || ConfigOnlyToolsets[c.Key] || !ToolsetAllowedForPlatform(c.Key, "cli") {
			continue
		}
		members := Default.ResolveStaticToolset(ctx, "", c.Key)
		inCLI, inACP := len(members) > 0, len(members) > 0
		for _, m := range members {
			inCLI = inCLI && cli[m]
			inACP = inACP && acp[m]
		}
		if inCLI && !inACP {
			saved := RecentlyShippedToolsets
			RecentlyShippedToolsets = map[string]bool{c.Key: true}
			t.Cleanup(func() { RecentlyShippedToolsets = saved })
			return c.Key
		}
	}
	t.Fatal("no configurable toolset sits inside icebath-cli and outside icebath-acp")
	return ""
}

func savedListFromBefore(platform string) config.Tree {
	var keys []any
	var names []string
	for _, c := range ConfigurableToolsets {
		if !RecentlyShippedToolsets[c.Key] && !DefaultOffToolsets[c.Key] && !ConfigOnlyToolsets[c.Key] && ToolsetAllowedForPlatform(c.Key, platform) {
			names = append(names, c.Key)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		keys = append(keys, n)
	}
	return config.Tree{"platform_toolsets": map[string]any{platform: keys}}
}

// The saved list from before the shipped toolset with agent.disabled_toolsets set to disabled must not enable it
func disabledToolsetsStillWin(t *testing.T, shipped string, disabled any) {
	t.Helper()
	cfg := savedListFromBefore("cli")
	cfg["agent"] = map[string]any{"disabled_toolsets": disabled}
	if platformTools(cfg, "cli", false)[shipped] {
		t.Fatalf("agent.disabled_toolsets %v must win", disabled)
	}
}

func TestRecentlyShippedToolsets(t *testing.T) {
	isolatePlatformResolver(t)
	shipped := withRecentlyShipped(t)
	t.Run("test_saved_list_gains_toolsets_that_shipped_after_it_was_written", func(t *testing.T) {
		onComposite := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli"}}}, "cli", false)
		if !onComposite[shipped] || !platformTools(savedListFromBefore("cli"), "cli", false)[shipped] {
			t.Fatalf("saved list must gain %s", shipped)
		}
	})
	t.Run("test_unchecking_the_new_toolset_sticks", func(t *testing.T) {
		cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli"}}}
		enabled := platformTools(cfg, "cli", false)
		delete(enabled, shipped)
		if err := Default.SavePlatformTools(context.Background(), "", cfg, "cli", enabled, map[string]bool{}); err != nil {
			t.Fatal(err)
		}
		if platformTools(cfg, "cli", false)[shipped] {
			t.Fatalf("unchecking %s must stick", shipped)
		}
	})
	t.Run("test_agent_disabled_toolsets_still_wins", func(t *testing.T) {
		disabledToolsetsStillWin(t, shipped, []any{shipped})
	})
	t.Run("test_agent_disabled_toolsets_json_array_string_form_still_wins", func(t *testing.T) {
		asJSON, _ := json.Marshal([]string{shipped})
		disabledToolsetsStillWin(t, shipped, string(asJSON))
	})
	t.Run("test_agent_disabled_toolsets_python_literal_string_form_still_wins", func(t *testing.T) {
		disabledToolsetsStillWin(t, shipped, "['"+shipped+"']")
	})
	t.Run("test_platforms_whose_composite_excludes_it_are_left_narrow", func(t *testing.T) {
		if platformTools(savedListFromBefore("acp"), "acp", false)[shipped] {
			t.Fatalf("acp composite never carried %s", shipped)
		}
	})
}

func TestDisabledCompositePrunesConstituentToolsets(t *testing.T) {
	isolatePlatformResolver(t)
	ctx := context.Background()
	cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli"}}, "agent": map[string]any{"disabled_toolsets": []any{"debugging"}}}
	enabled := platformTools(cfg, "cli", false)
	t.Run("test_disabled_composite_debugging_prunes_constituent_platform_toolsets", func(t *testing.T) {
		for _, n := range []string{"terminal", "file", "web"} {
			if enabled[n] {
				t.Fatalf("%s must be pruned: %v", n, enabled)
			}
		}
	})
	t.Run("test_disabled_composite_display_matches_runtime_tool_selection", func(t *testing.T) {
		runtime, _ := Default.SelectToolNames(ctx, nil, sortedSet(enabled), []string{"debugging"}, SelectionContext{})
		for _, name := range []string{"terminal", "file", "web", "vision", "skills"} {
			hit := false
			for _, tool := range Default.ResolveToolset(ctx, "", name) {
				hit = hit || runtime[tool]
			}
			if enabled[name] != hit {
				t.Fatalf("%s display %v runtime %v", name, enabled[name], hit)
			}
		}
	})
}

func TestExplicitPluginToolsetAdmitted(t *testing.T) {
	isolatePlatformResolver(t)
	t.Run("test_explicit_plugin_toolset_admitted_in_platform_toolsets", func(t *testing.T) {
		PluginToolsetCatalog.KeysNowait = func(context.Context) map[string]bool { return map[string]bool{"dplat_client": true} }
		cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli", "dplat_client"}}}
		if got := Default.PlatformTools(context.Background(), "", cfg, "cli", PlatformToolsOptions{IncludeDefaultMCPServers: true}); !got["dplat_client"] {
			t.Fatalf("plugin toolset dropped: %v", got)
		}
	})
}

func TestCompositeInjectsXSearchWithXAICredentials(t *testing.T) {
	isolatePlatformResolver(t)
	if platformTools(config.Tree{}, "cli", false)["x_search"] {
		t.Fatal("x_search without credentials")
	}
	XAIOAuthTokensReader = func(context.Context, string) error { return nil }
	if !platformTools(config.Tree{}, "cli", false)["x_search"] {
		t.Fatal("x_search must be injected with xAI OAuth tokens")
	}
	XAIOAuthTokensReader = func(context.Context, string) error { return errors.New("relogin") }
	t.Setenv("XAI_API_KEY", "  key  ")
	if !platformTools(config.Tree{}, "cli", false)["x_search"] {
		t.Fatal("x_search must be injected with XAI_API_KEY")
	}
}

func TestToolsConfigScalarHelpers(t *testing.T) {
	for _, c := range []struct {
		v    any
		def  bool
		want bool
	}{{true, false, true}, {0, true, false}, {2, false, true}, {" Yes ", false, true}, {"OFF", true, false}, {"maybe", true, true}, {1.0, false, false}, {nil, true, true}} {
		if got := ParseEnabledFlag(c.v, c.def); got != c.want {
			t.Fatalf("ParseEnabledFlag(%v, %v) = %v", c.v, c.def, got)
		}
	}
	for _, c := range []struct {
		cfg  config.Tree
		want bool
	}{{config.Tree{}, false}, {config.Tree{"context": map[string]any{"engine": " LCM "}}, true}, {config.Tree{"context": map[string]any{"engine": "Compressor"}}, false}, {config.Tree{"context": "lcm"}, false}, {config.Tree{"context": map[string]any{"engine": 0}}, false}} {
		if got := contextEngineActive(c.cfg); got != c.want {
			t.Fatalf("contextEngineActive(%v) = %v", c.cfg, got)
		}
	}
	if !pyContainsKanban("x,kanban") || !pyContainsKanban([]any{"kanban"}) || pyContainsKanban([]any{"web"}) || !pyContainsKanban(map[string]any{"kanban": nil}) {
		t.Fatal("kanban membership")
	}
	cfg := config.Tree{"image_gen": "corrupt"}
	cfgSection(cfg, "image_gen")["model"] = "m"
	cfgSection(cfg, "fresh")["k"] = 1
	if config.AsMap(cfg["image_gen"])["model"] != "m" || config.AsMap(cfg["fresh"])["k"] != 1 {
		t.Fatalf("cfgSection: %v", cfg)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	got, err := GetEnabledPlatforms(context.Background(), t.TempDir())
	if err != nil || !slices.Equal(got[:2], []string{"cli", "telegram"}) || slices.Contains(got, "discord") {
		t.Fatalf("GetEnabledPlatforms = %v, %v", got, err)
	}
}

// The _config helper of the shared metrics cases, a telemetry section holding one shared_metrics mapping
func sharedMetricsConfig(shared map[string]any) config.Tree {
	return config.Tree{"telemetry": map[string]any{"shared_metrics": shared}}
}

func TestSharedMetricsMenuAndChecklistDiff(t *testing.T) {
	t.Run("test_missing_telemetry_section_is_off", func(t *testing.T) {
		if enabled, send := sharedMetricsState(config.Tree{}); enabled || send {
			t.Fatalf("state %v %v", enabled, send)
		}
	})
	t.Run("test_malformed_section_does_not_raise", func(t *testing.T) {
		if enabled, send := sharedMetricsState(config.Tree{"telemetry": "nonsense"}); enabled || send {
			t.Fatalf("state %v %v", enabled, send)
		}
	})
	t.Run("test_reads_both_flags", func(t *testing.T) {
		if enabled, send := sharedMetricsState(sharedMetricsConfig(map[string]any{"enabled": true, "send": true})); !enabled || !send {
			t.Fatalf("state %v %v", enabled, send)
		}
	})
	t.Run("test_sending_state_names_the_destination", func(t *testing.T) {
		if label := sharedMetricsMenuLabel(sharedMetricsConfig(map[string]any{"enabled": true, "send": true})); !strings.Contains(label, "(sending to Nous)") {
			t.Fatalf("label %q", label)
		}
	})
	t.Run("test_retired_collect_only_answer_is_named_rather_than_read_off", func(t *testing.T) {
		if label := sharedMetricsMenuLabel(sharedMetricsConfig(map[string]any{"enabled": true, "send": false})); !strings.Contains(label, "(collecting locally)") {
			t.Fatalf("label %q", label)
		}
	})
	t.Run("test_off", func(t *testing.T) {
		if label := sharedMetricsMenuLabel(sharedMetricsConfig(map[string]any{"enabled": false, "send": false})); !strings.Contains(label, "(off)") {
			t.Fatalf("label %q", label)
		}
	})
	for _, c := range []struct {
		cfg  config.Tree
		want string
	}{
		{config.Tree{}, "Configure shared metrics  (off)"},
		{config.Tree{"telemetry": "on"}, "Configure shared metrics  (off)"},
		{config.Tree{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": "true", "send": true}}}, "Configure shared metrics  (off)"},
	} {
		if got := sharedMetricsMenuLabel(c.cfg); got != c.want {
			t.Fatalf("%v: %q", c.cfg, got)
		}
	}
	isolatePlatformResolver(t)
	added, removed := checklistDiff(context.Background(), map[string]bool{"web": true, "browser": true}, map[string]bool{"web": true, "terminal": true, "mcp-github": true}, "cli")
	if !maps.Equal(added, map[string]bool{"browser": true}) || !maps.Equal(removed, map[string]bool{"terminal": true}) {
		t.Fatalf("diff %v %v", added, removed)
	}
	t.Setenv("TAVILY_API_KEY", "k")
	t.Setenv("MISSING_KEY_X", "")
	row := pyjson.NewObject()
	row.Set("env_vars", []any{toolKey("TAVILY_API_KEY", "p", "")})
	if ok, err := providerEnvReady(context.Background(), t.TempDir(), row); !ok || err != nil {
		t.Fatalf("ready %v %v", ok, err)
	}
	row.Set("env_vars", []any{toolKey("TAVILY_API_KEY", "p", ""), toolKey("MISSING_KEY_X", "p", "")})
	if ok, _ := providerEnvReady(context.Background(), t.TempDir(), row); ok {
		t.Fatal("missing key must not be ready")
	}
	if ok, _ := providerEnvReady(context.Background(), t.TempDir(), pyjson.NewObject()); !ok {
		t.Fatal("row without keys is ready")
	}
}

func TestEnabledPluginToolsetsExportedMatchesTheResolver(t *testing.T) {
	cfg := map[string]any{"known_plugin_toolsets": map[string]any{"telegram": []any{"homeassistant"}}}
	keys := map[string]bool{"homeassistant": true, "spotify": true, "other": true}
	got := EnabledPluginToolsets(cfg, "telegram", []string{"web"}, keys)
	if !maps.Equal(got, enabledPluginToolsets(cfg, "telegram", []string{"web"}, keys)) || got["homeassistant"] || got["spotify"] || !got["other"] {
		t.Fatal(got)
	}
}
