package browser

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func freshProviderCache(t *testing.T) {
	t.Helper()
	resetCaches("")
	t.Cleanup(func() { resetCaches("") })
}

func cloudEnv(t *testing.T, home string, section map[string]any) *tools.Env {
	t.Helper()
	if home == "" {
		home = t.TempDir()
	}
	return &tools.Env{Home: home, Config: config.Tree{"browser": section}}
}

func resolutionCached(env *tools.Env) bool {
	cloudProviderMu.Lock()
	defer cloudProviderMu.Unlock()
	for k := range cachedProviders {
		if k.scope == cfgOf(env).homeKey() {
			return true
		}
	}
	return false
}

// TestCloudProviderCachePolicy test_cache_is_isolated_by_hermes_home
func TestCloudProviderCacheIsolatedByHome(t *testing.T) {
	freshProviderCache(t)
	root := t.TempDir()
	homeA, homeB := filepath.Join(root, "browser-a"), filepath.Join(root, "browser-b")
	providers := map[string]BrowserProvider{homeA: newFakeCloud("provider-a", true), homeB: newFakeCloud("provider-b", true)}
	var resolutions []string
	pin(t, &ensureBrowserPluginsLoaded, func(*tools.Env) {})
	pin(t, &cloudGetProvider, func(env *tools.Env, _ string) BrowserProvider {
		resolutions = append(resolutions, env.Home)
		return providers[env.Home]
	})
	section := map[string]any{"cloud_provider": "profile-provider"}
	resolveFor := func(h string) BrowserProvider {
		p, err := getCloudProvider(cloudEnv(t, h, section))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if resolveFor(homeA) != providers[homeA] || resolveFor(homeB) != providers[homeB] || resolveFor(homeA) != providers[homeA] {
		t.Fatal("a profile resolved another profile's provider")
	}
	if strings.Join(resolutions, ",") != homeA+","+homeB {
		t.Fatalf("resolutions %v", resolutions)
	}
}

// TestCloudProviderCachePolicy test_same_profile_registry_replacement_invalidates_cache
func TestCloudProviderRegistryReplacementInvalidatesCache(t *testing.T) {
	freshProviderCache(t)
	BrowserProviderRegistry.ResetForTests()
	t.Cleanup(BrowserProviderRegistry.ResetForTests)
	pin(t, &ensureBrowserPluginsLoaded, func(*tools.Env) {})
	env := cloudEnv(t, "", map[string]any{"cloud_provider": "cache-replacement"})
	first, second := newFakeCloud("cache-replacement", true), newFakeCloud("cache-replacement", true)
	if err := BrowserProviderRegistry.Register(first, env.Home); err != nil {
		t.Fatal(err)
	}
	if p, err := getCloudProvider(env); err != nil || p != first {
		t.Fatalf("first resolution %v %v", p, err)
	}
	if err := BrowserProviderRegistry.Register(second, env.Home); err != nil {
		t.Fatal(err)
	}
	if p, err := getCloudProvider(env); err != nil || p != second {
		t.Fatalf("replacement not picked up %v %v", p, err)
	}
}

// TestCloudProviderCachePolicy test_concurrent_registry_replacement_discards_stale_resolution
func TestCloudProviderConcurrentReplacementDiscardsStaleResolution(t *testing.T) {
	freshProviderCache(t)
	BrowserProviderRegistry.ResetForTests()
	t.Cleanup(BrowserProviderRegistry.ResetForTests)
	pin(t, &ensureBrowserPluginsLoaded, func(*tools.Env) {})
	env := cloudEnv(t, "", map[string]any{"cloud_provider": "cache-race"})
	first, second := newFakeCloud("cache-race", true), newFakeCloud("cache-race", true)
	paused, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	calls := 0
	original := cloudGetProvider
	pin(t, &cloudGetProvider, func(e *tools.Env, name string) BrowserProvider {
		calls++
		resolved := original(e, name)
		if calls == 1 {
			close(paused)
			select {
			case <-release:
			case <-time.After(2 * time.Second):
				t.Error("release never came")
			}
		}
		return resolved
	})
	if err := BrowserProviderRegistry.Register(first, env.Home); err != nil {
		t.Fatal(err)
	}
	result := make(chan BrowserProvider, 1)
	go func() {
		p, _ := getCloudProvider(env)
		result <- p
	}()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("resolution never paused")
	}
	if err := BrowserProviderRegistry.Register(second, env.Home); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case p := <-result:
		if p != second {
			t.Fatalf("stale resolution kept %v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolution never finished")
	}
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
}

// TestCloudProviderCachePolicy test_explicit_local_caches_permanently
func TestCloudProviderExplicitLocalCachesPermanently(t *testing.T) {
	freshProviderCache(t)
	env := cloudEnv(t, "", map[string]any{"cloud_provider": "local"})
	if p, err := getCloudProvider(env); err != nil || p != nil {
		t.Fatalf("local resolved %v %v", p, err)
	}
	if !resolutionCached(env) {
		t.Fatal("explicit local not cached")
	}
	env.Config = config.Tree{"browser": map[string]any{"cloud_provider": "browser-use"}}
	if p, err := getCloudProvider(env); err != nil || p != nil {
		t.Fatalf("cache did not stick %v %v", p, err)
	}
}

// TestCloudProviderCachePolicy test_no_credentials_yet_does_not_cache_none
func TestCloudProviderNoCredentialsDoesNotCacheNone(t *testing.T) {
	freshProviderCache(t)
	env := cloudEnv(t, "", map[string]any{})
	buUnconfigured, bbUnconfigured := newFakeCloud("browser-use", false), newFakeCloud("browserbase", false)
	pin(t, &cloudAutodetectSet, func() []func() BrowserProvider {
		return []func() BrowserProvider{func() BrowserProvider { return buUnconfigured }, func() BrowserProvider { return bbUnconfigured }}
	})
	if p, err := getCloudProvider(env); err != nil || p != nil {
		t.Fatalf("unconfigured resolved %v %v", p, err)
	}
	if resolutionCached(env) {
		t.Fatal("transient none was cached")
	}
	healed := newFakeCloud("healed-provider", true)
	pin(t, &cloudAutodetectSet, func() []func() BrowserProvider {
		return []func() BrowserProvider{func() BrowserProvider { return healed }, func() BrowserProvider { return bbUnconfigured }}
	})
	if p, err := getCloudProvider(env); err != nil || p != healed {
		t.Fatalf("healed provider not picked up %v %v", p, err)
	}
	if !resolutionCached(env) {
		t.Fatal("resolved provider not cached")
	}
}

// TestCloudProviderCachePolicy test_explicit_provider_instantiation_failure_does_not_cache
func TestCloudProviderInstantiationFailureDoesNotCache(t *testing.T) {
	freshProviderCache(t)
	pin(t, &ensureBrowserPluginsLoaded, func(*tools.Env) {})
	pin(t, &cloudGetProvider, func(*tools.Env, string) BrowserProvider { panic("missing dependency") })
	var logs bytes.Buffer
	env := cloudEnv(t, "", map[string]any{"cloud_provider": "browser-use"})
	env.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if p, err := getCloudProvider(env); err != nil || p != nil {
		t.Fatalf("exploding factory resolved %v %v", p, err)
	}
	if resolutionCached(env) {
		t.Fatal("failed instantiation was cached")
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "browser-use") {
		t.Fatalf("warning missing %q", out)
	}
}

func TestCloudProviderUnregisteredExplicitNameRaisesValueError(t *testing.T) {
	freshProviderCache(t)
	pin(t, &ensureBrowserPluginsLoaded, func(*tools.Env) {})
	pin(t, &cloudGetProvider, func(*tools.Env, string) BrowserProvider { return nil })
	env := cloudEnv(t, "", map[string]any{"cloud_provider": "Nope"})
	_, err := getCloudProvider(env)
	if !isValueError(err) || !strings.Contains(err.Error(), "'nope'") || !strings.Contains(err.Error(), "no registered browser plugin has that name") {
		t.Fatalf("err %v", err)
	}
	if resolutionCached(env) {
		t.Fatal("raised resolution cached")
	}
	if name, err := browserBackendName(env, ""); err == nil || name != "" {
		t.Fatalf("backend name swallowed the ValueError %q %v", name, err)
	}
}

func TestNousSelectionResolvesBrowserUse(t *testing.T) {
	freshProviderCache(t)
	pin(t, &ensureBrowserPluginsLoaded, func(*tools.Env) {})
	var asked []string
	bu := newFakeCloud("browser-use", true)
	pin(t, &cloudGetProvider, func(_ *tools.Env, name string) BrowserProvider { asked = append(asked, name); return bu })
	env := cloudEnv(t, "", map[string]any{"cloud_provider": " NOUS "})
	if p, err := getCloudProvider(env); err != nil || p != bu || strings.Join(asked, ",") != "browser-use" {
		t.Fatalf("nous routed %v %v %v", p, err, asked)
	}
	if name, err := browserBackendName(env, ""); err != nil || name != "browser-use" {
		t.Fatalf("backend %q %v", name, err)
	}
	if name, _ := browserBackendName(env, "lightpanda"); name != "lightpanda" {
		t.Fatalf("local engine label %q", name)
	}
}

func TestBrowserBackendNamePrecedence(t *testing.T) {
	freshProviderCache(t)
	env := cloudEnv(t, "", map[string]any{"cloud_provider": "local", "engine": "lightpanda"})
	if name, _ := browserBackendName(env, ""); name != "lightpanda" {
		t.Fatalf("engine label %q", name)
	}
	env.Config = config.Tree{"browser": map[string]any{"cloud_provider": "local"}}
	if name, _ := browserBackendName(env, ""); name != "local" {
		t.Fatalf("local label %q", name)
	}
	env.Config = config.Tree{"browser": map[string]any{"cloud_provider": "local", "cdp_url": "ws://127.0.0.1:9/x"}}
	if name, _ := browserBackendName(env, "lightpanda"); name != "cdp" {
		t.Fatalf("cdp label %q", name)
	}
	if isLocalBackend(env) {
		t.Fatal("a CDP override was trusted as local")
	}
}

// TestGetBrowserEngine test_config_lightpanda
func TestGetBrowserEngineConfigLightpanda(t *testing.T) {
	if got := cfgOf(browserEnv(map[string]any{"engine": "lightpanda"})).engine(); got != "lightpanda" {
		t.Fatalf("engine %q", got)
	}
}

// TestGetCommandTimeoutRace test_returns_default_when_config_read_raises
func TestCommandTimeoutDefaultWithoutConfig(t *testing.T) {
	s := cfgOf(&tools.Env{})
	if s.commandTimeout() != defaultCommandTimeout || s.commandTimeout() != defaultCommandTimeout {
		t.Fatal("unreadable config did not answer the default")
	}
}

// test_multiplex_tool_memo_scope test_config_caches_are_keyed_by_profile browser reads
func TestCommandTimeoutFollowsEachProfile(t *testing.T) {
	root := t.TempDir()
	load := func(name string, timeout int) *tools.Env {
		h := filepath.Join(root, name)
		if err := os.MkdirAll(h, 0o700); err != nil {
			t.Fatal(err)
		}
		body := "browser:\n  command_timeout: " + map[int]string{33: "33", 77: "77"}[timeout] + "\n"
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		tree, err := config.Load(h, nil)
		if err != nil {
			t.Fatal(err)
		}
		return &tools.Env{Home: h, Config: tree}
	}
	a, b := load("profA", 33), load("profB", 77)
	if cfgOf(a).commandTimeout() != 33 || cfgOf(b).commandTimeout() != 77 || cfgOf(a).commandTimeout() != 33 {
		t.Fatal("command timeout crossed profiles")
	}
}

func clearCloudEnv(t *testing.T) {
	for _, k := range []string{"BROWSER_USE_API_KEY", "BROWSERBASE_API_KEY", "BROWSERBASE_PROJECT_ID", "TERMINAL_ENV", "BROWSER_CDP_URL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// test_managed_browserbase_and_modal test_browser_use_explicit_local_mode_stays_local_even_when_managed_gateway_is_ready
func TestExplicitLocalStaysLocalWithManagedGatewayReady(t *testing.T) {
	freshProviderCache(t)
	clearCloudEnv(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "nous-token")
	t.Setenv("BROWSER_USE_GATEWAY_URL", "http://127.0.0.1:3009")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("browser:\n  cloud_provider: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := config.Load(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Home: h, Config: tree}
	if p, err := getCloudProvider(env); err != nil || p != nil {
		t.Fatalf("provider %v %v", p, err)
	}
	if !isLocalBackend(env) {
		t.Fatal("explicit local mode is not a local backend")
	}
}

// test_managed_browserbase_and_modal test_browserbase_does_not_use_gateway_only_configuration
func TestBrowserbaseIgnoresGatewayOnlyConfiguration(t *testing.T) {
	clearCloudEnv(t)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "nous-token")
	t.Setenv("BROWSERBASE_GATEWAY_URL", "http://127.0.0.1:3009")
	if newBrowserbaseProvider().IsAvailable(testEnv(t)) {
		t.Fatal("browserbase available from a gateway only configuration")
	}
}

func browserUseCase(t *testing.T, key, selection string, gw *tools.ManagedToolGatewayConfig) (*tools.Env, *int) {
	clearCloudEnv(t)
	if key != "" {
		t.Setenv("BROWSER_USE_API_KEY", key)
	}
	calls := 0
	pin(t, &browserUseReadSelection, func(*tools.Env, string) string { return selection })
	pin(t, &browserUseResolveGateway, func(*tools.Env, string, func(string) (string, error), func(*tools.Env) string) (*tools.ManagedToolGatewayConfig, error) {
		calls++
		return gw, nil
	})
	return testEnv(t), &calls
}

// TestBrowserUseStrictSelection test_nous_selection_routes_managed_even_with_direct_key
func TestBrowserUseNousSelectionRoutesManaged(t *testing.T) {
	env, _ := browserUseCase(t, "bu-key", "nous", &tools.ManagedToolGatewayConfig{Vendor: "browser-use", GatewayOrigin: "https://gw.example/", NousUserToken: "managed-token", ManagedMode: true})
	c, err := newBrowserUseProvider().configOrNone(env)
	if err != nil || c == nil || !c.ManagedMode || c.APIKey != "managed-token" {
		t.Fatalf("config %+v %v", c, err)
	}
}

// TestBrowserUseStrictSelection test_vendor_selection_missing_key_errors_without_managed_call
func TestBrowserUseVendorSelectionMissingKeyErrors(t *testing.T) {
	env, calls := browserUseCase(t, "", "browser-use", nil)
	_, err := newBrowserUseProvider().config(env)
	if !isValueError(err) || !strings.Contains(err.Error(), "browser is configured to use browser-use") || !strings.Contains(err.Error(), "BROWSER_USE_API_KEY") {
		t.Fatalf("err %v", err)
	}
	if *calls != 0 {
		t.Fatalf("managed gateway consulted %d times", *calls)
	}
}

// TestBrowserUseStrictSelection test_never_configured_key_still_routes_direct
func TestBrowserUseNeverConfiguredRoutesDirect(t *testing.T) {
	env, _ := browserUseCase(t, "bu-key", "", nil)
	c, err := newBrowserUseProvider().configOrNone(env)
	if err != nil || c == nil || c.ManagedMode || c.APIKey != "bu-key" {
		t.Fatalf("config %+v %v", c, err)
	}
}
