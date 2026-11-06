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

func TestPlatformToolsetWarnings(t *testing.T) {
	isolatePlatformResolver(t)
	buf := captureToolsConfigLog(t)
	discardPlatformToolsetsWarning("cli")
	platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"not-a-real-toolset"}}}, "cli", true)
	if out := buf.String(); !strings.Contains(out, "#38798") || !strings.Contains(out, "not-a-real-toolset") {
		t.Fatalf("all invalid list must warn, got %q", out)
	}
	for _, list := range [][]any{{"hermes-cli"}, {"hermes-cli", "bogus"}} {
		buf.Reset()
		discardPlatformToolsetsWarning("cli")
		platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": list}}, "cli", true)
		if strings.Contains(buf.String(), "#38798") {
			t.Fatalf("list %v must not warn, got %q", list, buf.String())
		}
	}
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
	for _, raw := range []any{nil, "bogus"} {
		got := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": raw}}, "cli", false)
		if !maps.Equal(got, def) {
			t.Fatalf("%v: got %v want %v", raw, got, def)
		}
	}
	discardPlatformToolsetsWarning("cli")
	buf.Reset()
	got := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": `["web", terminal`}}, "cli", false)
	if !maps.Equal(got, def) {
		t.Fatalf("malformed list string: got %v want %v", got, def)
	}
	if out := buf.String(); !strings.Contains(out, "platform_toolsets.cli") || !strings.Contains(out, "expected a YAML list") {
		t.Fatalf("malformed list string must warn, got %q", out)
	}
}

func TestDiscordToolsetsDoNotLeakAndLegacyAliasExpands(t *testing.T) {
	isolatePlatformResolver(t)
	got := platformTools(config.Tree{"platform_toolsets": map[string]any{"telegram": []any{"hermes-telegram", "discord"}}}, "telegram", true)
	if got["discord"] || got["discord_admin"] {
		t.Fatalf("discord leaked to telegram: %v", got)
	}
	enabled := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes", "kanban"}}}, "cli", false)
	ref := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli", "kanban"}}}, "cli", false)
	if len(enabled) == 0 || !maps.Equal(enabled, ref) {
		t.Fatalf("legacy alias with kanban: %v vs %v", enabled, ref)
	}
	alone := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes"}}}, "cli", false)
	ref = platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli", "hermes-api-server"}}}, "cli", false)
	if len(alone) == 0 || !maps.Equal(alone, ref) {
		t.Fatalf("legacy alias alone: %v vs %v", alone, ref)
	}
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
	for _, p := range Platforms {
		if _, ok := staticToolset(p.Info.DefaultToolset); !ok {
			t.Fatalf("platform %q references undefined toolset %q", p.Key, p.Info.DefaultToolset)
		}
		if p.Key == "cli" || p.Key == "api_server" || p.Key == "cron" {
			continue
		}
		if !slices.Contains(gateway.Includes, p.Info.DefaultToolset) {
			t.Fatalf("platform %q toolset %q missing from icebath-gateway includes", p.Key, p.Info.DefaultToolset)
		}
	}
}

func TestPlatformToolsMCPServerNames(t *testing.T) {
	isolatePlatformResolver(t)
	cfg := config.Tree{
		"platform_toolsets": map[string]any{"cli": []any{"web", 12306}},
		"mcp_servers":       map[string]any{"12306": map[string]any{"url": "https://example.com/mcp"}, "normal-server": map[string]any{"url": "https://example.com/mcp2"}},
	}
	if got := platformTools(cfg, "cli", true); !got["12306"] {
		t.Fatalf("numeric server name must survive as a string: %v", got)
	}
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

func TestRecentlyShippedToolsets(t *testing.T) {
	isolatePlatformResolver(t)
	shipped := withRecentlyShipped(t)
	onComposite := platformTools(config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli"}}}, "cli", false)
	if !onComposite[shipped] || !platformTools(savedListFromBefore("cli"), "cli", false)[shipped] {
		t.Fatalf("saved list must gain %s", shipped)
	}
	cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli"}}}
	enabled := platformTools(cfg, "cli", false)
	delete(enabled, shipped)
	if err := Default.SavePlatformTools(context.Background(), "", cfg, "cli", enabled, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if platformTools(cfg, "cli", false)[shipped] {
		t.Fatalf("unchecking %s must stick", shipped)
	}
	asJSON, _ := json.Marshal([]string{shipped})
	for _, disabled := range []any{[]any{shipped}, string(asJSON), "['" + shipped + "']"} {
		cfg := savedListFromBefore("cli")
		cfg["agent"] = map[string]any{"disabled_toolsets": disabled}
		if platformTools(cfg, "cli", false)[shipped] {
			t.Fatalf("agent.disabled_toolsets %v must win", disabled)
		}
	}
	if platformTools(savedListFromBefore("acp"), "acp", false)[shipped] {
		t.Fatalf("acp composite never carried %s", shipped)
	}
}

func TestDisabledCompositePrunesConstituentToolsets(t *testing.T) {
	isolatePlatformResolver(t)
	ctx := context.Background()
	cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli"}}, "agent": map[string]any{"disabled_toolsets": []any{"debugging"}}}
	enabled := platformTools(cfg, "cli", false)
	for _, n := range []string{"terminal", "file", "web"} {
		if enabled[n] {
			t.Fatalf("%s must be pruned: %v", n, enabled)
		}
	}
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
}

func TestExplicitPluginToolsetAdmitted(t *testing.T) {
	isolatePlatformResolver(t)
	PluginToolsetCatalog.KeysNowait = func(context.Context) map[string]bool { return map[string]bool{"dplat_client": true} }
	cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"hermes-cli", "dplat_client"}}}
	if got := Default.PlatformTools(context.Background(), "", cfg, "cli", PlatformToolsOptions{IncludeDefaultMCPServers: true}); !got["dplat_client"] {
		t.Fatalf("plugin toolset dropped: %v", got)
	}
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

func TestSharedMetricsMenuAndChecklistDiff(t *testing.T) {
	for _, c := range []struct {
		cfg  config.Tree
		want string
	}{
		{config.Tree{}, "Configure shared metrics  (off)"},
		{config.Tree{"telemetry": "on"}, "Configure shared metrics  (off)"},
		{config.Tree{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": "true", "send": true}}}, "Configure shared metrics  (off)"},
		{config.Tree{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": true}}}, "Configure shared metrics  (collecting locally)"},
		{config.Tree{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": true, "send": true}}}, "Configure shared metrics  (sending to Nous)"},
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
