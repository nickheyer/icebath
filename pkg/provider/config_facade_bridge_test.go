package provider

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs fn under a multiplex flag and, when secrets is non nil, a bound secret scope
func withFacadeScope(t *testing.T, secrets map[string]string, multiplex bool, fn func(ctx context.Context)) {
	t.Helper()
	SetMultiplexActive(multiplex)
	defer SetMultiplexActive(false)
	ctx := context.Background()
	if secrets != nil {
		ctx = WithSecretScope(ctx, secrets, "")
	}
	fn(ctx)
}

func TestGetEnvValueScope(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("OPENAI_API_KEY", "sk-other-profile")
	withFacadeScope(t, map[string]string{"OPENAI_API_KEY": "sk-this-profile"}, true, func(ctx context.Context) {
		if v, ok, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY"); err != nil || !ok || v != "sk-this-profile" {
			t.Fatalf("scoped value must win, got %q %v %v", v, ok, err)
		}
	})
	withFacadeScope(t, map[string]string{}, true, func(ctx context.Context) {
		if v, ok, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY"); err != nil || ok {
			t.Fatalf("a multiplexed scope miss must not leak environ, got %q %v %v", v, ok, err)
		}
	})
	t.Setenv("OPENAI_API_KEY", "sk-from-shell")
	withFacadeScope(t, map[string]string{}, false, func(ctx context.Context) {
		if v, _, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY"); err != nil || v != "sk-from-shell" {
			t.Fatalf("multiplex off falls back to environ, got %q %v", v, err)
		}
	})
	withFacadeScope(t, nil, false, func(ctx context.Context) {
		if v, _, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY"); err != nil || v != "sk-from-shell" {
			t.Fatalf("no scope behaves like a single profile, got %q %v", v, err)
		}
	})
	withFacadeScope(t, nil, true, func(ctx context.Context) {
		var unscoped *UnscopedSecretError
		if _, _, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY"); !errors.As(err, &unscoped) {
			t.Fatalf("an unscoped multiplexed read must fail closed, got %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("MY_TEST_KEY=from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("MY_TEST_KEY")
	withFacadeScope(t, nil, false, func(ctx context.Context) {
		if v, _, err := config.GetEnvValue(ctx, h, "MY_TEST_KEY"); err != nil || v != "from-dotenv" {
			t.Fatalf("dotenv fallback, got %q %v", v, err)
		}
	})
}

func TestCoreDeclaredEnvNamesExcludePlatformInjection(t *testing.T) {
	core, err := config.CoreDeclaredEnvNames()
	if err != nil {
		t.Fatal(err)
	}
	for name := range config.StaticCoreEnvNames() {
		if !core[name] {
			t.Fatalf("static name %s missing", name)
		}
	}
	for name := range providerEnvNames {
		if !core[name] {
			t.Fatalf("provider name %s missing", name)
		}
	}
	if _, err := config.PlatformManifestSecretEnvs(t.TempDir(), config.PlatformSourceUser, true); err != nil {
		t.Fatalf("the bridge registers core names, got %v", err)
	}
}

func TestStaticDeclaredEnvNamesSkipPlatformInjectedNames(t *testing.T) {
	config.OptionalEnvVars.Set("CHATX_INJECTED_SIGNING_SECRET", config.EnvVar{Password: true, Category: "messaging"})
	t.Cleanup(func() { config.OptionalEnvVars.Delete("CHATX_INJECTED_SIGNING_SECRET") })
	got, want := staticDeclaredEnvNames(), config.StaticCoreEnvNames()
	if got["CHATX_INJECTED_SIGNING_SECRET"] || len(got) == 0 || !maps.Equal(got, want) {
		t.Fatalf("static names %d, core %d, injected %v", len(got), len(want), got["CHATX_INJECTED_SIGNING_SECRET"])
	}
}

// The v38 step drops only the removed Relay plugin keys through the plugins lane hook
func TestFacadeMigrationDropsOnlyLegacyRelayPluginKeys(t *testing.T) {
	h := camofoxFresh(t)
	writeConfigYAML(t, h, "_config_version: 37\nplugins:\n  enabled:\n  - keep-me\n  - observability/nemo_relay\n  - nemo_relay\n")
	config.ResetCache()
	res, err := config.MigrateConfig(home.WithOverride(context.Background(), h), h, false, true)
	if err != nil {
		t.Fatal(err)
	}
	plugins, _ := camofoxConfig(t, h)["plugins"].(map[string]any)
	if enabled, _ := plugins["enabled"].([]any); !slices.Equal(enabled, []any{"keep-me"}) {
		t.Fatalf("plugins %v", plugins)
	}
	hits := 0
	for _, w := range res.Warnings {
		if strings.Contains(w, "ICEBATH_NEMO_RELAY_PLUGINS_TOML") {
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("warnings %v", res.Warnings)
	}
}

func TestExplicitToolsetKeysUnionsConfigurableAndPluginKeys(t *testing.T) {
	prev := tools.PluginToolsetCatalog.KeysNowait
	tools.PluginToolsetCatalog.KeysNowait = func(context.Context) map[string]bool { return map[string]bool{"myplug": true} }
	t.Cleanup(func() { tools.PluginToolsetCatalog.KeysNowait = prev })
	got := explicitToolsetKeys()
	if !got["myplug"] {
		t.Fatalf("plugin key missing from %v", got)
	}
	for k := range tools.ConfigurableKeys() {
		if !got[k] {
			t.Fatalf("configurable key %q missing", k)
		}
	}
	if len(got) != len(tools.ConfigurableKeys())+1 {
		t.Fatalf("exact union, got %d keys", len(got))
	}
}

func TestConfigFacadeStampHooksAndBrowserEnvSourcesExports(t *testing.T) {
	hooks := ConfigFacadeStampHooks()
	if hooks.RecordCoreUse == nil || hooks.AdoptAgentBrowserEnv == nil || hooks.WorkerSnapshotActive == nil {
		t.Fatal("stamp hooks incomplete")
	}
	sources := ConfigFacadeBrowserEnvSources(t.TempDir())
	if sources.HomeDotenv == nil || sources.IsLaunchHome == nil || sources.PMStoreRoots == nil || sources.DeferStartupNotice == nil {
		t.Fatal("browser env sources incomplete")
	}
}
