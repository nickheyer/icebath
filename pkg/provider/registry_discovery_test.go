package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Writes a plugin dir with an optional manifest and an init module
func writeManifestPlugin(t *testing.T, dir, manifest string, withManifest bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if withManifest {
		if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "__init__.py"), []byte("# plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Installs a flat kind model-provider plugin under the home plugins dir
func installFlatProviderPlugin(t *testing.T, h, name string) {
	t.Helper()
	writeManifestPlugin(t, filepath.Join(h, "plugins", name), "name: "+name+"\nkind: model-provider\n", true)
}

// Plugin double registering one external process profile with an alias named after the dir
func externalProcessPluginProfiles(base string) map[string]func(string) ([]Profile, error) {
	out := map[string]func(string) ([]Profile, error){}
	for _, name := range []string{"scaleup-only", "late-install", "installed-acp", "nested-acp", "other-standalone", "manifestless", "broken-manifest"} {
		name := name
		out[name] = func(string) ([]Profile, error) {
			return []Profile{{Name: name, Aliases: []string{name + "-alias"}, AuthType: AuthTypeExternalProcess, BaseURL: base + name, Mode: "chat_completions"}}, nil
		}
	}
	return out
}

// Reruns process wide discovery against a cleared registry, restored when the test ends
func rediscoverProviders(t *testing.T, ctx context.Context) {
	t.Helper()
	resetProviderRegistry(t)
	regDiscovered.Store(false)
	discoverProviders(ctx)
}

// Entry point scan double returning the given entry points
func fakeEntryPoints(t *testing.T, eps ...ProviderEntryPoint) {
	t.Helper()
	prev := installedProviderEntryPointsFn
	installedProviderEntryPointsFn = func(context.Context) ([]ProviderEntryPoint, error) { return eps, nil }
	t.Cleanup(func() { installedProviderEntryPointsFn = prev })
}

// Writes plugins.enabled and plugins.disabled into the home config
func enablePlugins(t *testing.T, h string, enabled, disabled []string) {
	t.Helper()
	body := "plugins:\n  enabled: [" + strings.Join(enabled, ", ") + "]\n  disabled: [" + strings.Join(disabled, ", ") + "]\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func callableEntryPoint(name string, p Profile) ProviderEntryPoint {
	return ProviderEntryPoint{Name: name, Value: name + ":register", Load: func(context.Context) (ProviderEntryPointTarget, error) {
		return ProviderEntryPointTarget{Callable: true, Invoke: func(context.Context) ([]Profile, error) { return []Profile{p}, nil }}, nil
	}}
}

func TestBundledPluginDirsAllRegisterProfiles(t *testing.T) {
	snapshotProviderRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	names := map[string]bool{}
	for _, p := range ListProviders(ctx) {
		names[p.Name] = true
	}
	if len(names) < len(bundledPluginDirs) {
		t.Fatalf("expected at least %d profiles, got %d", len(bundledPluginDirs), len(names))
	}
	for _, d := range bundledPluginDirs {
		if len(d.names) == 0 {
			t.Fatalf("%s registers nothing", d.dir)
		}
		for _, n := range d.names {
			if !names[n] {
				t.Fatalf("%s profile %s not listed", d.dir, n)
			}
		}
	}
}

func TestUserPluginOverridesBundled(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeManifestPlugin(t, filepath.Join(h, "plugins", "model-providers", "gmi"), "name: gmi-user-override\nkind: model-provider\nversion: 0.0.1\n", true)
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){"gmi": func(string) ([]Profile, error) {
		return []Profile{{Name: "gmi", Aliases: []string{"gmi-user-override-test"}, KeyEnv: []string{"GMI_API_KEY"},
			BaseURL: "https://user-override.example.com/v1", AuthType: AuthTypeAPIKey}}, nil
	}})
	gmi, ok := GetProviderProfile(context.Background(), "gmi")
	if !ok || gmi.BaseURL != "https://user-override.example.com/v1" || !containsString(gmi.Aliases, "gmi-user-override-test") {
		t.Fatalf("user override not applied: %+v", gmi)
	}
}

func TestSecondaryProfilePluginResolvesForItsHomeOnly(t *testing.T) {
	snapshotProviderRegistry(t)
	root := t.TempDir()
	launch, secondary := filepath.Join(root, "launch"), filepath.Join(root, "profiles", "scaleup")
	for _, d := range []string{launch, secondary} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", launch)
	fakeExternalProviderPlugins(t, externalProcessPluginProfiles("process://"))
	installFlatProviderPlugin(t, secondary, "scaleup-only")
	bg := context.Background()
	bound := home.WithOverride(bg, secondary)
	if getProviderProfile(bg, "scaleup-only") != nil {
		t.Fatal("launch home must not see the secondary plugin")
	}
	if getProviderProfile(bound, "scaleup-only") == nil || getProviderProfile(bound, "scaleup-only-alias") == nil {
		t.Fatal("secondary home resolves its plugin and alias")
	}
	if src := ProviderSource(bound, "scaleup-only"); src != "user" {
		t.Fatalf("source = %q", src)
	}
	if !containsString(listedNames(ListProviders(bound)), "scaleup-only") {
		t.Fatal("listed in its home")
	}
	env := testEnv(t, secondary)
	env.Ctx = bound
	if got, err := resolveProvider(env, "scaleup-only"); err != nil || got != "scaleup-only" {
		t.Fatalf("resolve_provider = %q %v", got, err)
	}
	if getProviderProfile(bg, "scaleup-only") != nil || containsString(listedNames(ListProviders(bg)), "scaleup-only") {
		t.Fatal("launch home stays clean")
	}
}

func TestPluginInstalledAfterDiscoveryIsFoundWithoutARestart(t *testing.T) {
	snapshotProviderRegistry(t)
	launch := t.TempDir()
	t.Setenv("ICEBATH_HOME", launch)
	fakeExternalProviderPlugins(t, externalProcessPluginProfiles("process://"))
	ctx := context.Background()
	if getProviderProfile(ctx, "late-install") != nil {
		t.Fatal("not installed yet")
	}
	installFlatProviderPlugin(t, launch, "late-install")
	expireHomeLayerChecks()
	if getProviderProfile(ctx, "late-install") == nil || !containsString(listedNames(ListProviders(ctx)), "late-install") {
		t.Fatal("late install found without a restart")
	}
}

func TestFlatInstalledModelProviderPluginsAreDiscoveredAlongsideNestedOnes(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	fakeExternalProviderPlugins(t, externalProcessPluginProfiles("acp://"))
	writeManifestPlugin(t, filepath.Join(h, "plugins", "installed-acp"), "name: installed-acp\nkind: \"model-provider\"\n", true)
	writeManifestPlugin(t, filepath.Join(h, "plugins", "model-providers", "nested-acp"), "name: nested-acp\nkind: model-provider\n", true)
	ctx := context.Background()
	if p := getProviderProfile(ctx, "installed-acp"); p == nil || p.BaseURL != "acp://installed-acp" {
		t.Fatalf("flat plugin: %+v", p)
	}
	if getProviderProfile(ctx, "installed-acp-alias") == nil || getProviderProfile(ctx, "nested-acp") == nil {
		t.Fatal("alias and nested plugin")
	}
}

func TestOtherPluginsInTheFlatDirectoryAreLeftToThePluginManager(t *testing.T) {
	snapshotProviderRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	calls := fakeExternalProviderPlugins(t, externalProcessPluginProfiles("acp://"))
	writeManifestPlugin(t, filepath.Join(h, "plugins", "other-standalone"), "name: other-standalone\nkind: standalone\n", true)
	writeManifestPlugin(t, filepath.Join(h, "plugins", "manifestless"), "", false)
	writeManifestPlugin(t, filepath.Join(h, "plugins", "broken-manifest"), "kind: [this is: not valid\n", true)
	ctx := context.Background()
	for _, p := range ListProviders(ctx) {
		if p.Name == "other-standalone" || p.Name == "manifestless" || p.Name == "broken-manifest" {
			t.Fatalf("%s must not register", p.Name)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("non provider plugins imported: %v", *calls)
	}
	if getProviderProfile(ctx, "openrouter") == nil {
		t.Fatal("bundled set intact")
	}
}

func TestEntryPointCallableAndModuleTargets(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	enablePlugins(t, h, []string{"ep-callable", "ep-module"}, nil)
	module := ProviderEntryPoint{Name: "ep-module", Value: "ep_module", Load: func(context.Context) (ProviderEntryPointTarget, error) {
		return ProviderEntryPointTarget{Profiles: []Profile{{Name: "ep-module", BaseURL: "https://b.test/v1"}}, Invoke: func(context.Context) ([]Profile, error) {
			t.Fatal("a non callable target is never invoked")
			return nil, nil
		}}, nil
	}}
	fakeEntryPoints(t, callableEntryPoint("ep-callable", Profile{Name: "ep-callable", Aliases: []string{"epc"}, BaseURL: "https://a.test/v1"}), module)
	ctx := context.Background()
	rediscoverProviders(t, ctx)
	for _, n := range []string{"ep-callable", "epc", "ep-module"} {
		if getProviderProfile(ctx, n) == nil {
			t.Fatalf("%s not registered", n)
		}
	}
}

func TestEntryPointNotEnabledOrDisabledIsSkipped(t *testing.T) {
	for _, tc := range []struct{ enabled, disabled []string }{
		{[]string{"some-other-plugin"}, nil},
		{[]string{"ep-callable"}, []string{"ep-callable"}},
	} {
		h := t.TempDir()
		t.Setenv("ICEBATH_HOME", h)
		enablePlugins(t, h, tc.enabled, tc.disabled)
		fakeEntryPoints(t, callableEntryPoint("ep-callable", Profile{Name: "ep-callable", BaseURL: "https://a.test/v1"}))
		ctx := context.Background()
		rediscoverProviders(t, ctx)
		if getProviderProfile(ctx, "ep-callable") != nil {
			t.Fatalf("%v/%v: entry point must be skipped", tc.enabled, tc.disabled)
		}
	}
}

func TestGeneralPluginRegisterCtxNotInvoked(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	enablePlugins(t, h, []string{"general-plugin"}, nil)
	calls := 0
	fakeEntryPoints(t, ProviderEntryPoint{Name: "general-plugin", Value: "general:register", Load: func(context.Context) (ProviderEntryPointTarget, error) {
		return ProviderEntryPointTarget{Callable: true, RequiresArguments: true, Invoke: func(context.Context) ([]Profile, error) {
			calls++
			return nil, nil
		}}, nil
	}})
	rediscoverProviders(t, context.Background())
	if calls != 0 {
		t.Fatal("a register(ctx) target is never invoked")
	}
}

func TestEntryPointFailureIsIsolated(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	enablePlugins(t, h, []string{"broken", "ep-callable"}, nil)
	broken := ProviderEntryPoint{Name: "broken", Value: "broken", Load: func(context.Context) (ProviderEntryPointTarget, error) {
		return ProviderEntryPointTarget{}, errors.New("broken plugin")
	}}
	fakeEntryPoints(t, broken, callableEntryPoint("ep-callable", Profile{Name: "ep-callable", BaseURL: "https://a.test/v1"}))
	ctx := context.Background()
	rediscoverProviders(t, ctx)
	if getProviderProfile(ctx, "ep-callable") == nil {
		t.Fatal("a broken entry point must not block the next one")
	}
}

func TestFilesystemPluginsWinOverEntryPoints(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	enablePlugins(t, h, []string{"openrouter"}, nil)
	fakeEntryPoints(t, callableEntryPoint("openrouter", Profile{Name: "openrouter", BaseURL: "https://impostor.test/v1"}))
	ctx := context.Background()
	rediscoverProviders(t, ctx)
	p := getProviderProfile(ctx, "openrouter")
	if p == nil || strings.Contains(p.BaseURL, "impostor.test") {
		t.Fatalf("bundled openrouter must win: %+v", p)
	}
}
