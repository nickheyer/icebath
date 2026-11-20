package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Fresh attempt memo and none of the provider env names in the process environment
func leftCoreRowsFresh(t *testing.T) {
	t.Helper()
	leftCoreFresh(t)
	for _, name := range []string{"ICEBATH_INFERENCE_PROVIDER", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "ACME_CLI_PATH"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// Home with optional .env, config.yaml, auth.json and cron jobs.json content
func leftCoreRowsHome(t *testing.T, root, env, cfg string, auth map[string]any, jobs []any) string {
	t.Helper()
	h := leftCoreHome(t, root, "home", env, cfg)
	if auth != nil {
		data, err := json.Marshal(auth)
		if err != nil {
			t.Fatal(err)
		}
		writeLeftCoreFile(t, filepath.Join(h, "auth.json"), string(data))
	}
	if jobs != nil {
		data, err := json.Marshal(map[string]any{"jobs": jobs})
		if err != nil {
			t.Fatal(err)
		}
		writeLeftCoreFile(t, filepath.Join(h, "cron", "jobs.json"), string(data))
	}
	return h
}

// Reports a home with a model route through the acme provider
func acmeRouted(h string, _ bool) bool {
	for _, slot := range leftCoreRouteSlots(h) {
		if strings.EqualFold(strings.TrimSpace(pyOrStr(slot["provider"])), "acme") {
			return true
		}
	}
	return false
}

// Fake row for a provider plugin, the generic seams run through it
func acmeRow() LeftCoreFeature {
	return LeftCoreFeature{
		Plugin: "acme-llm", Label: "Acme LLM", Unchanged: "your ACME_KEY is unchanged", InUse: acmeRouted,
		Providers: []string{"acme", "acme-acp"}, ProfileEnv: []string{"ACME_CLI_PATH"}, Notice: noLeftCoreNotice,
	}
}

// Import failure of a named module, what a provider plugin whose dependency is missing raises on load
type ModuleNotFoundError struct{ Name string }

func (e *ModuleNotFoundError) Error() string { return "No module named '" + e.Name + "'" }

// Home choosing the acme provider bound as the process home with the table reduced to the acme row
func acmeHome(t *testing.T, row LeftCoreFeature) (string, context.Context) {
	t.Helper()
	leftCoreRowsFresh(t)
	t.Cleanup(SwapLeftCoreTableForTests([]LeftCoreFeature{row}))
	h := leftCoreRowsHome(t, t.TempDir(), "", "model:\n  provider: acme\n", nil, nil)
	ctx := bindLeftCoreHome(t, h, true, nil)
	snapshotProviderRegistry(t)
	return h, ctx
}

func TestUnknownLeftCoreProviderNamesTheInstallOnEverySurface(t *testing.T) {
	h, ctx := acmeHome(t, acmeRow())
	env := PoolEnv{Ctx: ctx, Config: func() map[string]any { return leftCoreReadConfig(h) }}
	_, err := resolveProvider(env, "acme-acp")
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("resolve error %T %v", err, err)
	}
	exit := (&authCLI{ctx: ctx, env: env, ns: map[string]any{}, out: io.Discard, errw: io.Discard}).unknownProviderExit("acme")
	for _, text := range []string{err.Error(), exit.Error(), unknownProviderMessage(ctx, "ACME")} {
		if !strings.Contains(text, "Acme LLM moved out of core into the 'acme-llm' plugin") || !strings.Contains(text, "icebath plugins install acme-llm") {
			t.Fatalf("surface %q", text)
		}
	}
	mkPluginDir(t, h, "acme-llm")
	writeLeftCoreFile(t, filepath.Join(h, "config.yaml"), "model:\n  provider: acme\nplugins:\n  disabled: [acme-llm]\n")
	if hint := ProviderInstallHint(ctx, "acme"); !strings.Contains(hint, "icebath plugins enable acme-llm") {
		t.Fatalf("disabled hint %q", hint)
	}
	if hint := ProviderInstallHint(ctx, "other"); hint != "" {
		t.Fatal(hint)
	}
}

func TestAProviderPluginThatFailedToLoadGetsItsErrorNotEnable(t *testing.T) {
	for _, listed := range []string{"", "plugins:\n  enabled: [acme-llm]\n"} {
		h, ctx := acmeHome(t, acmeRow())
		plugin := filepath.Join(h, "plugins", "acme-llm")
		writeLeftCoreFile(t, filepath.Join(plugin, "plugin.yaml"), "name: acme-llm\nkind: model-provider\n")
		writeLeftCoreFile(t, filepath.Join(plugin, "__init__.py"), "from acme_sdk import client\n")
		writeLeftCoreFile(t, filepath.Join(h, "config.yaml"), "model:\n  provider: acme\n"+listed)
		fakeExternalProviderPlugins(t, map[string]func(homeKey string) ([]Profile, error){
			"acme-llm": func(string) ([]Profile, error) { return nil, &ModuleNotFoundError{Name: "acme_sdk"} },
		})
		withPluginHooks(t, func(hk *PluginManagerHooks) { hk.LoadError = func(p string) string { return PluginLoadError(ctx, p) } })
		homeLayerFor(ctx, true)
		hint := ProviderInstallHint(ctx, "acme")
		if !strings.Contains(hint, "ModuleNotFoundError: No module named 'acme_sdk'") || !strings.Contains(hint, "icebath plugins update acme-llm") || strings.Contains(hint, "enable") {
			t.Fatalf("listed %q hint %q", listed, hint)
		}
	}
}

func TestRuntimeResolutionInstallsThePluginFirstForAHomeUsingIt(t *testing.T) {
	h, ctx := acmeHome(t, acmeRow())
	calls := 0
	leftCoreInstallInto = func(context.Context, string, *bool) func(string) (map[string]any, error) {
		return func(string) (map[string]any, error) {
			calls++
			return map[string]any{"ok": false, "error": "x"}, nil
		}
	}
	recoverLeftCoreProvider(ctx, h, "acme")
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
	clearLeftCoreAttempted()
	recoverLeftCoreProvider(ctx, h, "openrouter")
	writeLeftCoreFile(t, filepath.Join(h, "config.yaml"), "model:\n  provider: deepseek\n")
	clearLeftCoreAttempted()
	recoverLeftCoreProvider(ctx, h, "acme")
	if calls != 1 {
		t.Fatalf("a home that never chose it installed, calls %d", calls)
	}
	registerTestProfile(t, Profile{Name: "acme", DisplayName: "Acme", AuthType: "api_key"})
	writeLeftCoreFile(t, filepath.Join(h, "config.yaml"), "model:\n  provider: acme\n")
	clearLeftCoreAttempted()
	recoverLeftCoreProvider(ctx, h, "acme")
	if calls != 1 {
		t.Fatalf("a served provider installed, calls %d", calls)
	}
}

func TestLaunchEnvIsRefusedByEnvWritersWhileNoProfileDeclaresIt(t *testing.T) {
	for _, pluginDir := range []bool{false, true} {
		row := acmeRow()
		row.ProfileEnv = []string{"ACME_CLI_PATH", "ACME_BASE_URL"}
		row.LaunchEnv = []string{"ACME_CLI_PATH"}
		h, _ := acmeHome(t, row)
		if pluginDir {
			mkPluginDir(t, h, "acme-llm")
		}
		if err := config.ValidateEnvNameForWrite("ACME_CLI_PATH"); err == nil || !strings.Contains(err.Error(), "denylist") {
			t.Fatalf("plugin dir %v: %v", pluginDir, err)
		}
		if err := config.ValidateEnvNameForWrite("ACME_BASE_URL"); err != nil {
			t.Fatalf("plugin dir %v: %v", pluginDir, err)
		}
	}
}

func TestCopilotLaunchNamesStayRefusedAndItsBaseURLWritable(t *testing.T) {
	for _, pluginDir := range []bool{false, true} {
		leftCoreRowsFresh(t)
		h := leftCoreRowsHome(t, t.TempDir(), "", "", nil, nil)
		bindLeftCoreHome(t, h, true, nil)
		snapshotProviderRegistry(t)
		if pluginDir {
			mkPluginDir(t, h, "github-copilot")
		}
		for _, name := range []string{"ICEBATH_COPILOT_ACP_COMMAND", "ICEBATH_COPILOT_ACP_ARGS", "COPILOT_CLI_PATH"} {
			if err := config.ValidateEnvNameForWrite(name); err == nil || !strings.Contains(err.Error(), "denylist") {
				t.Fatalf("plugin dir %v %s: %v", pluginDir, name, err)
			}
		}
		if err := config.ValidateEnvNameForWrite("COPILOT_ACP_BASE_URL"); err != nil {
			t.Fatalf("plugin dir %v: %v", pluginDir, err)
		}
	}
}

func TestModelsDevNeverStandsInForAProviderWhosePluginLeftCore(t *testing.T) {
	_, ctx := acmeHome(t, acmeRow())
	prev := modelsDevInfoFn
	modelsDevInfoFn = func(_ context.Context, canonical string, _ bool) *ModelsDevProvider {
		if canonical == "acme" {
			return &ModelsDevProvider{Env: []string{"ACME_TOKEN"}, Name: "Acme", API: "https://api.acme.test"}
		}
		return nil
	}
	t.Cleanup(func() { modelsDevInfoFn = prev })
	if def := GetProviderDef(ctx, "acme", true); def != nil {
		t.Fatalf("acme %+v", def)
	}
	if def := GetProviderDef(ctx, "acme-acp", true); def != nil {
		t.Fatalf("acme-acp %+v", def)
	}
	registerTestProfile(t, Profile{Name: "acme", DisplayName: "Acme", AuthType: "api_key"})
	if def := GetProviderDef(ctx, "acme", true); def == nil || def.BaseURL != "https://api.acme.test" {
		t.Fatalf("loaded plugin %+v", def)
	}
}

func TestARowWithoutNoticeAddsNothing(t *testing.T) {
	f := acmeRow()
	f.Notice = noLeftCoreNotice
	if got := leftCoreNoticed("x", t.TempDir(), &f, false); got != "x" {
		t.Fatal(got)
	}
}
