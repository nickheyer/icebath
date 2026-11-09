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

var ghOnlyAuth = map[string]any{"credential_pool": map[string]any{"copilot": []any{map[string]any{"source": "gh_cli", "label": "gh auth token", "access_token": "t"}}}}

func TestCopilotInUseIsAnyProviderSlotNamingIt(t *testing.T) {
	leftCoreRowsFresh(t)
	rows := []struct {
		cfg, env string
		auth     map[string]any
		jobs     []any
	}{
		{"model:\n  provider: copilot\n  default: gpt-5.4\n", "", nil, nil},
		{"model:\n  provider: GitHub-Copilot\n", "", nil, nil},
		{"model:\n  provider: copilot-acp\n", "", nil, nil},
		{"fallback_providers:\n  - provider: copilot\n    model: gpt-5.4\n", "", nil, nil},
		{"fallback_model:\n  provider: github-models\n  model: gpt-4.1\n", "", nil, nil},
		{"auxiliary:\n  vision:\n    provider: copilot\n", "", nil, nil},
		{"delegation:\n  provider: copilot-acp\n", "", nil, nil},
		{"delegation:\n  fallback_providers:\n    - provider: copilot\n      model: gpt-5.4\n", "", nil, nil},
		{"moa:\n  presets:\n    mine:\n      reference_models:\n        - provider: copilot\n          model: gpt-5.4\n" +
			"      aggregator:\n        provider: nous\n        model: hermes-4\n", "", nil, nil},
		{"", "ICEBATH_INFERENCE_PROVIDER=copilot\n", nil, nil},
		{"", "", map[string]any{"active_provider": "copilot"}, nil},
		{"", "", nil, []any{map[string]any{"id": "j1", "provider": "copilot", "model": "gpt-5.4"}}},
		{"cron:\n  model: gpt-5.4\n  model_provider: copilot\n", "", nil, nil},
		{"delegation:\n  backend: model_map\n  backends:\n    model_map:\n      options:\n        - name: fast\n" +
			"          provider: copilot\n          model: gpt-5.4\n", "", nil, nil},
		{"delegation:\n  backends:\n    model_map:\n      options:\n        - name: deep\n          provider: nous\n" +
			"          fallback_providers:\n            - provider: github-copilot\n              model: gpt-5.4\n", "", nil, nil},
	}
	for i, row := range rows {
		if !copilotInUse(leftCoreRowsHome(t, t.TempDir(), row.env, row.cfg, row.auth, row.jobs), false) {
			t.Fatalf("row %d not in use", i)
		}
	}
}

func TestCopilotCredentialsAloneAreNotUse(t *testing.T) {
	leftCoreRowsFresh(t)
	rows := []struct {
		cfg, env string
		auth     map[string]any
	}{
		{"model:\n  provider: nous\n", "GH_TOKEN=gho_x\nCOPILOT_GITHUB_TOKEN=gho_y\n", nil},
		{"model:\n  provider: openrouter\n  default: github/copilot-like\n", "", nil},
		{"", "", map[string]any{"credential_pool": map[string]any{"copilot": []any{map[string]any{"source": "gh_cli", "access_token": "t"}}}}},
		{"model: copilot\n", "", nil},
		{"model:\n  provider: github\ncustom_providers:\n  - name: github\n    base_url: https://llm.example.test/v1\n", "", nil},
		{"model:\n  provider: github-models\nproviders:\n  github-models:\n    base_url: https://llm.example.test/v1\n", "", nil},
	}
	for i, row := range rows {
		if copilotInUse(leftCoreRowsHome(t, t.TempDir(), row.env, row.cfg, row.auth, nil), false) {
			t.Fatalf("row %d in use", i)
		}
	}
}

func TestProcessEnvProviderCountsOnlyForTheLaunchHome(t *testing.T) {
	leftCoreRowsFresh(t)
	h := leftCoreRowsHome(t, t.TempDir(), "", "model:\n  provider: nous\n", nil, nil)
	bindLeftCoreHome(t, h, true, nil)
	t.Setenv("ICEBATH_INFERENCE_PROVIDER", "copilot")
	if copilotInUse(h, false) || !copilotInUse(h, true) {
		t.Fatal(copilotInUse(h, false), copilotInUse(h, true))
	}
}

func TestReloginNoticeOnlyForACopilotHomeWithoutATokenThePluginReads(t *testing.T) {
	leftCoreRowsFresh(t)
	rows := []struct {
		cfg, env string
		auth     map[string]any
		notice   bool
	}{
		{"model:\n  provider: copilot\n", "", ghOnlyAuth, true},
		{"model:\n  provider: copilot\n", "", nil, true},
		{"model:\n  provider: copilot\n", "GITHUB_TOKEN=gho_x\n", ghOnlyAuth, false},
		{"model:\n  provider: copilot\n", "", map[string]any{"credential_pool": map[string]any{"copilot": []any{map[string]any{"source": "manual"}}}}, false},
		{"model:\n  provider: copilot\n", "", map[string]any{"credential_pool": map[string]any{"copilot": []any{map[string]any{"source": "manual:device_code"}}}}, false},
		{"model:\n  provider: copilot-acp\n", "", nil, false},
		{"model:\n  provider: nous\n", "", ghOnlyAuth, false},
	}
	for i, row := range rows {
		text := copilotReloginNotice(leftCoreRowsHome(t, t.TempDir(), row.env, row.cfg, row.auth, nil), false)
		if strings.Contains(text, "icebath auth add copilot") != row.notice {
			t.Fatalf("row %d notice %q", i, text)
		}
	}
}

func TestGhOnlyHomeIsToldToLogInOnEveryMigrationOutcome(t *testing.T) {
	leftCoreRowsFresh(t)
	h := leftCoreRowsHome(t, t.TempDir(), "", "model:\n  provider: copilot\n", ghOnlyAuth, nil)
	var said []string
	say := func(m string) { said = append(said, m) }
	install := func(name string) (map[string]any, error) {
		mkPluginDir(t, h, name)
		return map[string]any{"ok": true}, nil
	}
	if got := MigrateLeftCoreHome(context.Background(), h, install, say, false); len(got) != 1 || got[0] != "github-copilot" {
		t.Fatalf("installed %v", got)
	}
	if !strings.Contains(said[0], "✓ GitHub Copilot moved out of core") || !strings.Contains(said[0], "run `icebath auth add copilot`") {
		t.Fatalf("said %q", said)
	}
	other := leftCoreRowsHome(t, filepath.Join(t.TempDir(), "b"), "", "model:\n  provider: copilot\n", ghOnlyAuth, nil)
	said = nil
	MigrateLeftCoreHome(context.Background(), other, func(string) (map[string]any, error) {
		return map[string]any{"ok": false, "error": "network down"}, nil
	}, say, false)
	if !strings.Contains(said[0], "could not be installed automatically") || !strings.Contains(said[0], "run `icebath auth add copilot`") {
		t.Fatalf("said %q", said)
	}
}

func TestUpdateReadsTheLaunchHomesExportedTokenForTheReloginNotice(t *testing.T) {
	leftCoreRowsFresh(t)
	root := t.TempDir()
	launch := leftCoreHome(t, root, filepath.Join("launch", "launch"), "", "model:\n  provider: copilot\n")
	other := leftCoreHome(t, root, filepath.Join("other", "other"), "", "model:\n  provider: copilot\n")
	bindLeftCoreHome(t, launch, true, nil)
	t.Setenv("GH_TOKEN", "gho_exported")
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.DependencyHomes = func() ([]string, error) { return []string{launch, other}, nil }
	})
	leftCoreInstallInto = func(_ context.Context, h string, _ *bool) func(string) (map[string]any, error) {
		return func(name string) (map[string]any, error) {
			mkPluginDir(t, h, name)
			return map[string]any{"ok": true}, nil
		}
	}
	var said []string
	if _, err := MigrateAllLeftCoreHomes(context.Background(), func(m string) { said = append(said, m) }); err != nil {
		t.Fatal(err)
	}
	byHome := map[string]string{}
	for _, h := range []string{launch, other} {
		var lines []string
		for _, m := range said {
			if strings.Contains(m, PluginHomeLabel(h)) {
				lines = append(lines, m)
			}
		}
		byHome[h] = strings.Join(lines, " ")
	}
	if !strings.Contains(byHome[launch], "✓ GitHub Copilot moved out of core") || strings.Contains(byHome[launch], "run `icebath auth add copilot`") {
		t.Fatalf("launch %q", byHome[launch])
	}
	if !strings.Contains(byHome[other], "run `icebath auth add copilot`") {
		t.Fatalf("other %q", byHome[other])
	}
}

// Fake row for a provider plugin, the generic seams run through it
func acmeRow() LeftCoreFeature {
	return LeftCoreFeature{
		Plugin: "acme-llm", Label: "Acme LLM", Unchanged: "your ACME_KEY is unchanged",
		InUse: func(h string, processEnv bool) bool {
			return ProvidersSelected(h, []string{"acme", "acme-acp"}, processEnv)["acme"]
		},
		Providers: []string{"acme", "acme-acp"}, ProfileEnv: []string{"ACME_CLI_PATH"}, Notice: noLeftCoreNotice,
	}
}

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
	writeLeftCoreFile(t, filepath.Join(h, "config.yaml"), "model:\n  provider: nous\n")
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

func TestARouteAtTheRowsHostIsUse(t *testing.T) {
	leftCoreRowsFresh(t)
	f := leftCoreFeatureByPlugin("github-copilot")
	for i, cfg := range []string{
		"model:\n  provider: custom\n  base_url: https://api.githubcopilot.com\n",
		"model:\n  provider: work\ncustom_providers:\n  - name: work\n    base_url: https://api.business.githubcopilot.com\n",
		"providers:\n  gh:\n    base_url: https://models.github.ai/inference\n",
		"auxiliary:\n  vision:\n    provider: custom\n    base_url: https://api.enterprise.githubcopilot.com/\n",
		"delegation:\n  provider: custom\n  base_url: https://api.githubcopilot.com\n",
		"fallback_providers:\n  - provider: custom\n    base_url: https://api.githubcopilot.com\n    model: gpt-5.4\n",
	} {
		if !FeatureInUse(f, leftCoreRowsHome(t, t.TempDir(), "", cfg, nil, nil), false) {
			t.Fatalf("row %d not in use", i)
		}
	}
}

func TestALookalikeHostIsNotUse(t *testing.T) {
	leftCoreRowsFresh(t)
	f := leftCoreFeatureByPlugin("github-copilot")
	for _, url := range []string{"https://githubcopilot.com.evil.test/v1", "https://evil.test/api.githubcopilot.com"} {
		if FeatureInUse(f, leftCoreRowsHome(t, t.TempDir(), "", "model:\n  provider: custom\n  base_url: "+url+"\n", nil, nil), false) {
			t.Fatalf("%s in use", url)
		}
	}
}
