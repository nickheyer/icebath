package dashboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One recorded background action spawn
type spawnCall struct {
	subcommand []string
	name       string
}

// Replaces the action spawner with a recorder for the case, a spawn carrying env overrides failing it
func spawnRecorder(t *testing.T) *[]spawnCall {
	t.Helper()
	calls := &[]spawnCall{}
	real := spawnHermesAction
	spawnHermesAction = func(_ context.Context, subcommand []string, name string, envOverrides map[string]string) (actionProc, error) {
		if envOverrides != nil {
			t.Errorf("spawn of %s carried env overrides %v", name, envOverrides)
		}
		*calls = append(*calls, spawnCall{append([]string(nil), subcommand...), name})
		return &fakeActionProc{pid: 4242}, nil
	}
	t.Cleanup(func() { spawnHermesAction = real })
	return calls
}

// Pins the cua driver install state the post setup predicate reads for the case
func cuaDriverInstalled(t *testing.T, ready bool) {
	t.Helper()
	real := tools.ToolsConfigHooks.CuaDriverInstallReady
	tools.ToolsConfigHooks.CuaDriverInstallReady = func(*tools.Env) (bool, error) { return ready, nil }
	t.Cleanup(func() { tools.ToolsConfigHooks.CuaDriverInstallReady = real })
}

func TestToggleToolsetInstallOnEnableEnableComputerUseSpawnsCuaInstallWhenBinaryMissing(t *testing.T) {
	c := toolsClient(t)
	calls := spawnRecorder(t)
	cuaDriverInstalled(t, false)
	r := c.send("PUT", "/api/tools/toolsets/computer_use", map[string]any{"enabled": true})
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	if data["enabled"] != true || data["post_setup_started"] != "cua_driver" {
		t.Fatalf("body %s", r.Body)
	}
	if len(*calls) == 0 {
		t.Fatal("expected a background post-setup spawn")
	}
	first := (*calls)[0]
	if n := len(first.subcommand); n < 3 || !reflect.DeepEqual(first.subcommand[n-3:], []string{"tools", "post-setup", "cua_driver"}) || first.name != "tools-post-setup" {
		t.Fatalf("spawn %v", first)
	}
}

func TestToggleToolsetInstallOnEnableEnableComputerUseSkipsInstallWhenBinaryPresent(t *testing.T) {
	c := toolsClient(t)
	calls := spawnRecorder(t)
	cuaDriverInstalled(t, true)
	r := c.send("PUT", "/api/tools/toolsets/computer_use", map[string]any{"enabled": true})
	if r.Status != 200 || jsonObject(t, r)["post_setup_started"] != nil || len(*calls) != 0 {
		t.Fatalf("status %d %s calls %v", r.Status, r.Body, *calls)
	}
}

func TestToggleToolsetInstallOnEnableDisableNeverSpawnsInstall(t *testing.T) {
	c := toolsClient(t)
	calls := spawnRecorder(t)
	cuaDriverInstalled(t, false)
	r := c.send("PUT", "/api/tools/toolsets/computer_use", map[string]any{"enabled": false})
	if r.Status != 200 || jsonObject(t, r)["post_setup_started"] != nil || len(*calls) != 0 {
		t.Fatalf("status %d %s calls %v", r.Status, r.Body, *calls)
	}
}

func TestToggleToolsetInstallOnEnableSpawnFailureDoesNotFailTheToggle(t *testing.T) {
	c := toolsClient(t)
	cuaDriverInstalled(t, false)
	real := spawnHermesAction
	spawnHermesAction = func(context.Context, []string, string, map[string]string) (actionProc, error) {
		return nil, errors.New("spawn exploded")
	}
	t.Cleanup(func() { spawnHermesAction = real })
	r := c.send("PUT", "/api/tools/toolsets/computer_use", map[string]any{"enabled": true})
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	if data["ok"] != true || data["enabled"] != true || data["post_setup_started"] != nil {
		t.Fatalf("body %s", r.Body)
	}
}

func TestToggleToolsetInstallOnEnableToolsetsWithoutPendingPostSetupUnchanged(t *testing.T) {
	c := toolsClient(t)
	calls := spawnRecorder(t)
	r := c.send("PUT", "/api/tools/toolsets/web", map[string]any{"enabled": true})
	if r.Status != 200 || jsonObject(t, r)["post_setup_started"] != nil || len(*calls) != 0 {
		t.Fatalf("status %d %s calls %v", r.Status, r.Body, *calls)
	}
}

// Takes find docker off the host so the probe cannot find a real runtime
func hideHostRuntimes(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_DOCKER_BINARY", "")
	t.Setenv("PATH", t.TempDir())
	t.Cleanup(tools.SwapDockerSearchPaths(nil))
	tools.ResetDockerExecutable()
	t.Cleanup(tools.ResetDockerExecutable)
}

func TestProbeReadyWhenOnlyPodmanIsConfigured(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	hideHostRuntimes(t)
	fake := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_DOCKER_BINARY", fake)
	var captured [][]string
	real := terminalRunCapture
	terminalRunCapture = func(_ context.Context, argv []string, _ time.Duration) (*tools.CaptureResult, error) {
		captured = append(captured, append([]string(nil), argv...))
		return &tools.CaptureResult{Code: 0, Stdout: "podman version 5.0.0\n"}, nil
	}
	t.Cleanup(func() { terminalRunCapture = real })
	status, detail := probeDockerBackend(context.Background())
	if status != "ready" || detail != "" || !reflect.DeepEqual(captured, [][]string{{fake, "version"}}) {
		t.Fatalf("%q %q %v", status, detail, captured)
	}
}

func TestProbeNeedsSetupWhenDockerAndPodmanAreAbsent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	hideHostRuntimes(t)
	status, detail := probeDockerBackend(context.Background())
	lower := strings.ToLower(detail)
	if status != "needs_setup" || !strings.Contains(lower, "not found") || !strings.Contains(lower, "podman") {
		t.Fatalf("%q %q", status, detail)
	}
}

const toolsetPluginSource = `
def register(ctx):
    ctx.register_tool(
        name="work_ping", toolset="worktools",
        schema={"name": "work_ping", "description": "ping", "parameters": {"type": "object", "properties": {}}},
        handler=lambda args, **kw: "pong")
`

const backendPluginSource = `
from agent.terminal_env_provider import TerminalEnvironmentProvider
class Box(TerminalEnvironmentProvider):
    @property
    def name(self):
        return "workbox"
    def is_available(self):
        return True
    def create_environment(self, **kw):
        raise NotImplementedError
def register(ctx):
    ctx.register_terminal_environment_provider(Box())
`

// Installs the provider registries of the production wiring for the case, the map of cmd/icebath/wiring.go kind for kind
func installProviderRegistries(t *testing.T) {
	t.Helper()
	previous := plugins.InstalledServices()
	plugins.InstallServices(&plugins.Services{DeclaresModelProviderKind: provider.DeclaresModelProviderKind, ProviderRegistries: map[string]plugins.ScopedRegistry{
		"image_gen_provider":            media.ImageGenScopedRegistry(),
		"video_gen_provider":            media.VideoGenScopedRegistry(),
		"tts_provider":                  media.TTSScopedRegistry(),
		"transcription_provider":        media.TranscriptionScopedRegistry(),
		"web_search_provider":           agent.WebSearchScopedRegistry(),
		"browser_provider":              browser.BrowserProviderRegistry.Scoped(),
		"browser_page_backend":          browser.BrowserPageBackendRegistry.Scoped(),
		"terminal_environment_provider": agent.TerminalEnvScopedRegistry(),
		"secret_source":                 provider.SecretSourceScopedRegistry(),
		"delegation_provider":           agent.DelegationScopedRegistry(),
		"login_backend":                 browser.LoginBackendScopedRegistry(),
	}})
	t.Cleanup(func() {
		plugins.InstallServices(previous)
		plugins.ResetPluginManagersForTests()
	})
	plugins.ResetPluginManagersForTests()
}

// Clears the process wide multi profile hosting latch before and after the case, the per test reset of tests/conftest.py
func resetMultiProfileLatch(t *testing.T) {
	t.Helper()
	provider.SetMultiplexActive(false)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
}

// Session client over the dashboard application with the production provider registries installed
func toolsClient(t *testing.T) sessionClient {
	t.Helper()
	c := newSessionClient(t)
	installProviderRegistries(t)
	return c
}

// Launch home and a work profile whose plugin directory alone holds the named plugin, no bundled plugins anywhere
func pluginProfileHomes(t *testing.T, plugin, source, rootConfig string) (sessionClient, string, string) {
	t.Helper()
	c, root := memoryProviderClient(t)
	work := filepath.Join(root, "profiles", "work")
	dir := filepath.Join(work, "plugins", plugin)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, text := range map[string]string{
		filepath.Join(dir, "plugin.yaml"):  "name: " + plugin + "\nversion: '1.0'\n",
		filepath.Join(dir, "__init__.py"):  source,
		filepath.Join(work, "config.yaml"): "plugins:\n  enabled:\n  - " + plugin + "\n",
		filepath.Join(root, "config.yaml"): rootConfig,
	} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", t.TempDir())
	installProviderRegistries(t)
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	return c, root, work
}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAToolsetInstalledOnlyInTheTargetProfileCanBeEnabledThere(t *testing.T) {
	c, root, work := pluginProfileHomes(t, "worktools", toolsetPluginSource, "model:\n  default: gpt-5\n")
	r := c.get("/api/tools/toolsets?profile=work")
	listed, _ := r.JSON(t).([]any)
	found := false
	for _, row := range listed {
		found = found || row.(*pyjson.Object).GetOr("name", nil) == "worktools"
	}
	if !found {
		t.Fatalf("status %d, worktools absent from %d toolset rows", r.Status, len(listed))
	}
	r = c.send("PUT", "/api/tools/toolsets/worktools?profile=work", map[string]any{"enabled": true})
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	doc, err := config.ReadUser(work)
	if err != nil {
		t.Fatal(err)
	}
	platformToolsets, _ := doc["platform_toolsets"].(map[string]any)
	cli, _ := platformToolsets["cli"].([]any)
	if !slices.Contains(cli, any("worktools")) {
		t.Fatalf("cli toolsets %v", cli)
	}
	before := readText(t, filepath.Join(root, "config.yaml"))
	r = c.send("PUT", "/api/tools/toolsets/worktools", map[string]any{"enabled": true})
	if r.Status != 400 || !strings.Contains(string(r.Body), "Unknown toolset") || readText(t, filepath.Join(root, "config.yaml")) != before {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

func TestEveryToolsetRouteResolvesTheNameInTheTargetProfile(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		body         map[string]any
		scopedAnswer string
	}{
		{"GET", "/api/tools/toolsets/worktools/config", nil, ""},
		{"PUT", "/api/tools/toolsets/worktools/env", map[string]any{"env": map[string]any{}}, ""},
		{"PUT", "/api/tools/toolsets/worktools/provider", map[string]any{"provider": "x"}, "no configurable category"},
		{"POST", "/api/tools/toolsets/worktools/post-setup", map[string]any{"key": "nope"}, "Unknown post-setup key"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			c, _, _ := pluginProfileHomes(t, "worktools", toolsetPluginSource, "model:\n  default: gpt-5\n")
			r := c.send(tc.method, tc.path+"?profile=work", tc.body)
			if strings.Contains(string(r.Body), "Unknown toolset") {
				t.Fatalf("status %d %s", r.Status, r.Body)
			}
			if tc.scopedAnswer == "" && r.Status != 200 {
				t.Fatalf("status %d %s", r.Status, r.Body)
			}
			if tc.scopedAnswer != "" && (r.Status != 400 || !strings.Contains(string(r.Body), tc.scopedAnswer)) {
				t.Fatalf("status %d %s", r.Status, r.Body)
			}
			r = c.send(tc.method, tc.path, tc.body)
			if r.Status != 400 || !strings.Contains(string(r.Body), "Unknown toolset") {
				t.Fatalf("status %d %s", r.Status, r.Body)
			}
		})
	}
}

func terminalBackendOf(t *testing.T, h string) any {
	t.Helper()
	doc, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	terminal, _ := doc["terminal"].(map[string]any)
	return terminal["backend"]
}

func TestABackendInstalledOnlyInTheTargetProfileIsSelectableThere(t *testing.T) {
	c, root, work := pluginProfileHomes(t, "workbox", backendPluginSource, "terminal:\n  backend: local\n")
	r := c.get("/api/tools/terminal/backends?profile=work")
	backends, _ := jsonObject(t, r)["backends"].([]any)
	found := false
	for _, row := range backends {
		found = found || row.(map[string]any)["name"] == "workbox"
	}
	if !found {
		t.Fatalf("status %d, workbox absent from %d backend rows", r.Status, len(backends))
	}
	r = c.send("PUT", "/api/tools/terminal/backend?profile=work", map[string]any{"backend": "workbox"})
	if r.Status != 200 || terminalBackendOf(t, work) != "workbox" {
		t.Fatalf("status %d %s backend %v", r.Status, r.Body, terminalBackendOf(t, work))
	}
	r = c.send("PUT", "/api/tools/terminal/backend", map[string]any{"backend": "workbox"})
	if r.Status != 400 || !strings.Contains(string(r.Body), "Unknown terminal backend") || terminalBackendOf(t, root) != "local" {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

// Toolset listing rows of the dashboard by name
func toolsetListing(t *testing.T, c sessionClient) map[string]*pyjson.Object {
	t.Helper()
	r := c.get("/api/tools/toolsets")
	rows, ok := r.JSON(t).([]any)
	if r.Status != 200 || !ok {
		t.Fatalf("toolsets status %d, %d bytes", r.Status, len(r.Body))
	}
	out := map[string]*pyjson.Object{}
	for _, raw := range rows {
		row := raw.(*pyjson.Object)
		out[row.GetOr("name", "").(string)] = row
	}
	return out
}

// Toolset names saved for a platform in the loaded config of a home, absent for a platform without an entry
func savedPlatformToolsets(t *testing.T, h, platform string) ([]any, bool) {
	t.Helper()
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := map[string]any(cfg)["platform_toolsets"].(map[string]any)
	names, held := saved[platform].([]any)
	return names, held
}

func TestNewEndpointsDiscordToolsetsReadAndWriteDiscordPlatform(t *testing.T) {
	c := toolsClient(t)
	h := os.Getenv("ICEBATH_HOME")
	listing := toolsetListing(t, c)
	discord := listing["discord"]
	if discord == nil || discord.GetOr("platform", nil) != "discord" || discord.GetOr("platform_label", nil) != "Discord" || discord.GetOr("enabled", nil) != false {
		raw, _ := pyjson.Dumps(discord)
		t.Fatalf("discord row %s", raw)
	}
	r := c.send("PUT", "/api/tools/toolsets/discord", map[string]any{"enabled": true})
	want := map[string]any{"ok": true, "name": "discord", "platform": "discord", "enabled": true, "post_setup_started": nil}
	if r.Status != 200 || !reflect.DeepEqual(jsonObject(t, r), want) {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	onDiscord, held := savedPlatformToolsets(t, h, "discord")
	if !held || !slices.Contains(onDiscord, any("discord")) {
		t.Fatalf("platform_toolsets.discord %v (present %v)", onDiscord, held)
	}
	if onCLI, _ := savedPlatformToolsets(t, h, "cli"); slices.Contains(onCLI, any("discord")) {
		t.Fatalf("discord saved on cli %v", onCLI)
	}
	listing = toolsetListing(t, c)
	if listing["discord"].GetOr("enabled", nil) != true || listing["discord_admin"].GetOr("enabled", nil) != false {
		t.Fatalf("after enable discord=%v discord_admin=%v", listing["discord"].GetOr("enabled", nil), listing["discord_admin"].GetOr("enabled", nil))
	}
	r = c.send("PUT", "/api/tools/toolsets/discord_admin", map[string]any{"enabled": true})
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	onDiscord, _ = savedPlatformToolsets(t, h, "discord")
	if !slices.Contains(onDiscord, any("discord")) || !slices.Contains(onDiscord, any("discord_admin")) {
		t.Fatalf("platform_toolsets.discord %v", onDiscord)
	}
}

func TestNewEndpointsGetToolsetConfigReturnsProviderMatrix(t *testing.T) {
	c := toolsClient(t)
	r := c.get("/api/tools/toolsets/tts/config")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	providers, isList := data["providers"].([]any)
	if data["name"] != "tts" || data["has_category"] != true || !isList || len(providers) == 0 {
		t.Fatalf("name %v has_category %v providers %d (list %v)", data["name"], data["has_category"], len(providers), isList)
	}
	activeProvider, hasActive := data["active_provider"]
	if !hasActive {
		t.Fatal("active_provider key absent")
	}
	names := map[any]bool{}
	var active []any
	for _, raw := range providers {
		prov := raw.(map[string]any)
		name, hasName := prov["name"]
		isActive, hasIsActive := prov["is_active"]
		envVars, envIsList := prov["env_vars"].([]any)
		if !hasName || !hasIsActive || !envIsList {
			t.Fatalf("provider row %v", prov)
		}
		for _, ev := range envVars {
			e := ev.(map[string]any)
			if _, ok := e["key"]; !ok {
				t.Fatalf("env var without key %v", e)
			}
			if _, ok := e["is_set"]; !ok {
				t.Fatalf("env var without is_set %v", e)
			}
		}
		names[name] = true
		if isActive == true {
			active = append(active, name)
		}
	}
	if activeProvider != nil && !names[activeProvider] {
		t.Fatalf("active_provider %v is no listed provider", activeProvider)
	}
	if len(active) > 0 && activeProvider != active[0] {
		t.Fatalf("active_provider %v, first active row %v", activeProvider, active[0])
	}
	if len(active) == 0 && activeProvider != nil {
		t.Fatalf("active_provider %v with no active row", activeProvider)
	}
}

// Logs the Nous Portal account out for the case through the lookup the subscription features read
func nousLoggedOut(t *testing.T) {
	t.Helper()
	previous := provider.SwapNousSubAccountInfo(func(provider.PoolEnv, bool) *provider.NousPortalAccountInfo {
		return &provider.NousPortalAccountInfo{LoggedIn: false, Source: "none", Fresh: false}
	})
	t.Cleanup(func() { provider.SwapNousSubAccountInfo(previous) })
}

func TestNewEndpointsGetToolsetConfigReportsTruthfulProviderStatus(t *testing.T) {
	c := toolsClient(t)
	nousLoggedOut(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("ELEVENLABS_API_KEY", "")
	t.Cleanup(tools.SwapPostSetupModuleImportable(func(context.Context, string) bool { return false }))
	r := c.get("/api/tools/toolsets/tts/config")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	providers, _ := jsonObject(t, r)["providers"].([]any)
	status := map[string]any{}
	valid := map[any]bool{"ready": true, "needs_keys": true, "needs_auth": true, "needs_setup": true}
	for _, raw := range providers {
		prov := raw.(map[string]any)
		if !valid[prov["status"]] {
			t.Fatalf("provider %v has status %v", prov["name"], prov["status"])
		}
		status[prov["name"].(string)] = prov["status"]
	}
	want := map[string]string{"Microsoft Edge TTS": "ready", "Nous Subscription": "needs_auth", "xAI TTS": "needs_auth",
		"KittenTTS": "needs_setup", "Piper": "needs_setup", "ElevenLabs": "needs_keys"}
	for name, wantStatus := range want {
		if status[name] != wantStatus {
			t.Errorf("%s status %v, want %s", name, status[name], wantStatus)
		}
	}
}

func TestNewEndpointsSelectManagedNousProviderReportsNeedsNousAuth(t *testing.T) {
	c := toolsClient(t)
	h := os.Getenv("ICEBATH_HOME")
	nousLoggedOut(t)
	r := c.send("PUT", "/api/tools/toolsets/browser/provider", map[string]any{"provider": "Nous Subscription (Browser Use cloud)"})
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	if data["ok"] != true || data["needs_nous_auth"] != true || data["feature"] != "browser" {
		t.Fatalf("body %s", r.Body)
	}
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	browserCfg, _ := map[string]any(cfg)["browser"].(map[string]any)
	if _, held := browserCfg["use_gateway"]; browserCfg["cloud_provider"] != "nous" || held {
		t.Fatalf("browser config cloud_provider=%v use_gateway present=%v", browserCfg["cloud_provider"], held)
	}
}

// Loaded config of a home with defaults merged, the load_config of the hermes cases
func loadedConfig(t *testing.T, h string) map[string]any {
	t.Helper()
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any(cfg)
}

func TestNewEndpointsSelectWebSearchBackendMatchesRuntimeResolution(t *testing.T) {
	c := toolsClient(t)
	h := os.Getenv("ICEBATH_HOME")
	t.Setenv("SEARXNG_URL", "http://localhost:8888")
	t.Setenv("FIRECRAWL_API_URL", "http://localhost:3002")
	base := c.send("PUT", "/api/tools/toolsets/web/provider", map[string]any{"provider": "Firecrawl Self-Hosted"})
	if base.Status != 200 {
		t.Fatalf("status %d %s", base.Status, base.Body)
	}
	r := c.send("PUT", "/api/tools/toolsets/web/provider", map[string]any{"provider": "SearXNG", "capability": "search"})
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if body := jsonObject(t, r); body["ok"] != true || body["capability"] != "search" {
		t.Fatalf("body %s", r.Body)
	}
	cfg := loadedConfig(t, h)
	web, _ := cfg["web"].(map[string]any)
	if web["search_backend"] != "searxng" || web["backend"] != "firecrawl" {
		t.Fatalf("web config search_backend=%v backend=%v", web["search_backend"], web["backend"])
	}
	env := &tools.Env{Home: h, Config: config.Tree(cfg)}
	if search, extract := tools.GetSearchBackend(env), tools.GetExtractBackend(env); search != "searxng" || extract != "firecrawl" {
		t.Fatalf("runtime search=%q extract=%q", search, extract)
	}
	data := jsonObject(t, c.get("/api/tools/toolsets/web/config"))
	if data["active_search_backend"] != "searxng" || data["active_extract_backend"] != "firecrawl" {
		t.Fatalf("endpoint search=%v extract=%v", data["active_search_backend"], data["active_extract_backend"])
	}
}

func TestNewEndpointsTerminalSshProbeReadyWhenConfigured(t *testing.T) {
	c := toolsClient(t)
	h := os.Getenv("ICEBATH_HOME")
	hideHostRuntimes(t)
	cfg := loadedConfig(t, h)
	terminal, ok := cfg["terminal"].(map[string]any)
	if !ok {
		terminal = map[string]any{}
		cfg["terminal"] = terminal
	}
	terminal["ssh_host"], terminal["ssh_user"] = "devbox.example.com", "hermes"
	if err := config.SaveConfig(context.Background(), h, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	body := jsonObject(t, c.get("/api/tools/terminal/backends"))
	backends, _ := body["backends"].([]any)
	var ssh map[string]any
	for _, raw := range backends {
		if row := raw.(map[string]any); row["name"] == "ssh" {
			ssh = row
			break
		}
	}
	detail, _ := ssh["detail"].(string)
	if ssh == nil || ssh["status"] != "ready" || !strings.Contains(detail, "hermes@devbox.example.com") {
		t.Fatalf("ssh row %v", ssh)
	}
}

func TestToolsConfigEndpointsSaveEnvWritesKeyAndValidatesAllowlist(t *testing.T) {
	c := toolsClient(t)
	h := os.Getenv("ICEBATH_HOME")
	cfg := jsonObject(t, c.get("/api/tools/toolsets/web/config"))
	key := ""
	providers, _ := cfg["providers"].([]any)
	for _, raw := range providers {
		envVars, _ := raw.(map[string]any)["env_vars"].([]any)
		if len(envVars) > 0 {
			key, _ = envVars[0].(map[string]any)["key"].(string)
		}
		if key != "" {
			break
		}
	}
	if key == "" {
		t.Skip("no env-var-bearing web provider in this build")
	}
	r := c.send("PUT", "/api/tools/toolsets/web/env", map[string]any{"env": map[string]any{key: "test-secret-123"}})
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	body := jsonObject(t, r)
	saved, _ := body["saved"].([]any)
	isSet, _ := body["is_set"].(map[string]any)
	if !slices.Contains(saved, any(key)) || isSet[key] != true {
		t.Fatalf("key %s saved %v is_set %v", key, saved, isSet[key])
	}
	value, _, err := config.GetEnvValue(context.Background(), h, key)
	if err != nil || value != "test-secret-123" {
		t.Fatalf("get_env_value(%s) = %q, %v", key, value, err)
	}
	if stored := config.LoadEnv(h)[key]; stored != "test-secret-123" {
		t.Fatalf(".env holds %q for %s", stored, key)
	}
}

func TestToolsConfigEndpointsPostSetupUnknownToolset400(t *testing.T) {
	c := toolsClient(t)
	r := c.send("POST", "/api/tools/toolsets/not_a_toolset/post-setup", map[string]any{"key": "agent_browser"})
	if r.Status != 400 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

// Default home and the named profile worker_beta, each with an empty config and the profile with an empty env file
func isolatedProfiles(t *testing.T) (sessionClient, string, string) {
	t.Helper()
	c := toolsClient(t)
	defaultHome := os.Getenv("ICEBATH_HOME")
	workerHome := filepath.Join(defaultHome, "profiles", "worker_beta")
	for _, h := range []string{defaultHome, workerHome} {
		if err := os.MkdirAll(h, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workerHome, ".env"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return c, defaultHome, workerHome
}

func TestProfileScopedPostSetupPostSetupSpawnsWithProfileFlag(t *testing.T) {
	c, _, _ := isolatedProfiles(t)
	calls := spawnRecorder(t)
	real := validPostSetupKeys
	validPostSetupKeys = func(context.Context) map[string]bool { return map[string]bool{"agent_browser": true} }
	t.Cleanup(func() { validPostSetupKeys = real })
	r := c.send("POST", "/api/tools/toolsets/browser/post-setup", map[string]any{"key": "agent_browser", "profile": "worker_beta"})
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0].subcommand, []string{"-p", "worker_beta", "tools", "post-setup", "agent_browser"}) {
		t.Fatalf("calls %v", *calls)
	}
}
