package tools

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Provider row of a tool category by its display name
func categoryRow(t *testing.T, category, name string) *pyjson.Object {
	t.Helper()
	for _, p := range catProviders(categoryOf(category)) {
		if rowText(p, "name") == name {
			return p
		}
	}
	t.Fatalf("no %s row named %q", category, name)
	return nil
}

// Row built from ordered key value pairs
func caseRow(pairs ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

// Lines of the captured output that start with the given print prefix
func printedWith(out, prefix string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

func anyContains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func section(cfg config.Tree, key string) map[string]any {
	m, _ := pyDictOf(cfg[key])
	return m
}

func TestPostSetupGateCuaDriverMissingForcesSetup(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	t.Setenv("PATH", t.TempDir())
	needs, err := s.toolsetNeedsConfigurationPrompt("computer_use", config.Tree{}, false)
	if err != nil || !needs {
		t.Fatalf("needs %v %v", needs, err)
	}
}

func TestPostSetupGateIncompatibleCuaDriverForcesSetup(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.CuaDriverInstallReady, func(*Env) (bool, error) { return false, nil })
	if needs, err := s.toolsetNeedsConfigurationPrompt("computer_use", config.Tree{}, false); err != nil || !needs {
		t.Fatalf("needs %v %v", needs, err)
	}
}

func TestPostSetupGateCompatibleCuaDriverSkipsSetup(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.CuaDriverInstallReady, func(*Env) (bool, error) { return true, nil })
	if needs, err := s.toolsetNeedsConfigurationPrompt("computer_use", config.Tree{}, false); err != nil || needs {
		t.Fatalf("needs %v %v", needs, err)
	}
}

func TestPostSetupGatePostSetupPredicateExceptionDoesNotBlock(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	prev := postSetupInstalled["cua_driver"]
	t.Cleanup(func() { postSetupInstalled["cua_driver"] = prev })
	postSetupInstalled["cua_driver"] = func(*ToolsSetup) (bool, error) { return false, os.ErrInvalid }
	if !s.PostSetupAlreadyInstalled("cua_driver") {
		t.Fatal("a failing predicate blocked setup")
	}
	postSetupInstalled["cua_driver"] = func(*ToolsSetup) (bool, error) { panic("predicate broken") }
	if !s.PostSetupAlreadyInstalled("cua_driver") {
		t.Fatal("a raising predicate blocked setup")
	}
}

func TestBrowserBackendPromptBrowserCloudProviderSetSkipsProviderPicker(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	cfg := config.Tree{"browser": map[string]any{"cloud_provider": "local"}}
	if needs, err := s.toolsetNeedsConfigurationPrompt("browser", cfg, false); err != nil || needs {
		t.Fatalf("needs %v %v", needs, err)
	}
}

func TestBrowserBackendPromptBrowserEmptyStillPrompts(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	if needs, err := s.toolsetNeedsConfigurationPrompt("browser", config.Tree{"browser": nil}, false); err != nil || !needs {
		t.Fatalf("needs %v %v", needs, err)
	}
}

// Configurator run over a home whose config.yaml holds body, the loaded config returned
func loaderHome(t *testing.T, body string) (*ToolsSetup, config.Tree) {
	t.Helper()
	s, _ := newTestToolsSetup(t, "")
	if err := os.WriteFile(filepath.Join(s.home(), "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg
}

func TestBrowserBackendPromptThroughLoaderUnsetBrowserStillPrompts(t *testing.T) {
	s, cfg := loaderHome(t, "cli: {}\n")
	if needs, err := s.toolsetNeedsConfigurationPrompt("browser", cfg, false); err != nil || !needs {
		t.Fatalf("needs %v %v", needs, err)
	}
}

func TestBrowserBackendPromptThroughLoaderExplicitCloudProviderSkipsPrompt(t *testing.T) {
	s, cfg := loaderHome(t, "browser:\n  cloud_provider: browser-use\n")
	if needs, err := s.toolsetNeedsConfigurationPrompt("browser", cfg, false); err != nil || needs {
		t.Fatalf("needs %v %v", needs, err)
	}
}

func TestConfigWritesWriteProviderConfigSetsSttProvider(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	cfg := config.Tree{"stt": map[string]any{"use_gateway": true}}
	if err := s.writeProviderConfig(categoryRow(t, "stt", "Groq"), cfg, ""); err != nil {
		t.Fatal(err)
	}
	stt := section(cfg, "stt")
	if stt["provider"] != "groq" {
		t.Fatalf("stt %v", stt)
	}
	if _, has := stt["use_gateway"]; has {
		t.Fatalf("use_gateway kept %v", stt)
	}
}

func TestConfigWritesApplyProviderSelectionStt(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(false, map[string]NousToolFeature{}, nil))
	cfg := config.Tree{}
	if err := s.ApplyProviderSelection("stt", "OpenAI", cfg); err != nil {
		t.Fatal(err)
	}
	if section(cfg, "stt")["provider"] != "openai" {
		t.Fatalf("stt %v", cfg["stt"])
	}
}

func TestActiveDetectionActiveMatchesConfig(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	cfg := config.Tree{"stt": map[string]any{"provider": "groq"}}
	if ok, err := s.IsProviderActive(categoryRow(t, "stt", "Groq"), cfg, false); err != nil || !ok {
		t.Fatalf("groq %v %v", ok, err)
	}
	if ok, err := s.IsProviderActive(categoryRow(t, "stt", "OpenAI"), cfg, false); err != nil || ok {
		t.Fatalf("openai %v %v", ok, err)
	}
}

func TestActiveDetectionUnsetProviderDefaultsToLocal(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	if ok, err := s.IsProviderActive(categoryRow(t, "stt", "Local Whisper"), config.Tree{}, false); err != nil || !ok {
		t.Fatalf("local %v %v", ok, err)
	}
}

func TestModelPickerConfigureSttModelDefaultsToCurrent(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	cfg := config.Tree{"stt": map[string]any{"openai": map[string]any{"model": "gpt-transcribe"}}}
	var defaults []int
	swapHook(t, &toolsPromptChoice, func(_ *ToolsSetup, _ string, _ []string, def int) (int, error) {
		defaults = append(defaults, def)
		return 0, nil
	})
	if err := s.configureSTTModel("openai", cfg); err != nil {
		t.Fatal(err)
	}
	catalog, _ := ToolsConfigHooks.STTModelCatalog("openai")
	if len(defaults) != 1 || defaults[0] != slices.Index(catalog, "gpt-transcribe") {
		t.Fatalf("defaults %v catalog %v", defaults, catalog)
	}
}

func TestConfigOnlyExclusionSttExcludedFromChecklistUniverse(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	keys := checklistToolsetKeys(context.Background(), "cli")
	if keys["stt"] || !keys["tts"] {
		t.Fatalf("universe %v", keys)
	}
}

func TestVideoPluginProviderActiveActiveWhenVideoGenProviderMatches(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	row := caseRow("name", "xAI Grok Imagine", "video_gen_plugin_name", "xai")
	if ok, err := s.IsProviderActive(row, config.Tree{"video_gen": map[string]any{"provider": "xai"}}, false); err != nil || !ok {
		t.Fatalf("active %v %v", ok, err)
	}
}

func TestVideoPluginProviderActiveDetectActiveIndexPicksVideoPluginMatch(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	t.Setenv("FAL_KEY", "fal-key")
	cfg := config.Tree{"video_gen": map[string]any{"provider": "xai"}}
	providers := []*pyjson.Object{
		caseRow("name", "xAI Grok Imagine", "env_vars", []any{}, "video_gen_plugin_name", "xai"),
		caseRow("name", "FAL.ai", "env_vars", []any{caseRow("key", "FAL_KEY", "prompt", "FAL")}, "video_gen_plugin_name", "fal"),
	}
	if idx, err := s.detectActiveProviderIndex(providers, cfg, false); err != nil || idx != 0 {
		t.Fatalf("index %d %v", idx, err)
	}
}

// Selector run with the model pickers answering their default, the hermes _quiet fixture
func quietSelectors(t *testing.T) *ToolsSetup {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &toolsPromptChoice, func(_ *ToolsSetup, _ string, _ []string, def int) (int, error) { return def, nil })
	return s
}

func TestImageGenSelectorPreservesManagedSelection(t *testing.T) {
	s := quietSelectors(t)
	cfg := config.Tree{}
	if err := s.writeProviderConfig(caseRow("image_gen_plugin_name", "fal"), cfg, "image_gen"); err != nil {
		t.Fatal(err)
	}
	imageGen := section(cfg, "image_gen")
	if _, has := imageGen["use_gateway"]; imageGen["provider"] != "nous" || has {
		t.Fatalf("managed write %v", imageGen)
	}
	if err := s.selectPluginImageGenProvider("fal", cfg, true); err != nil {
		t.Fatal(err)
	}
	imageGen = section(cfg, "image_gen")
	if _, has := imageGen["use_gateway"]; imageGen["provider"] != "nous" || has {
		t.Fatalf("selector clobbered %v", imageGen)
	}
}

func TestImageGenSelectorDirectKeyPickWritesVendor(t *testing.T) {
	s := quietSelectors(t)
	cfg := config.Tree{"image_gen": map[string]any{"use_gateway": true}}
	if err := s.selectPluginImageGenProvider("fal", cfg, false); err != nil {
		t.Fatal(err)
	}
	imageGen := section(cfg, "image_gen")
	if _, has := imageGen["use_gateway"]; imageGen["provider"] != "fal" || has {
		t.Fatalf("direct pick %v", imageGen)
	}
}

func TestImageAndVideoSelectorsShareTheSelectionContract(t *testing.T) {
	s := quietSelectors(t)
	for _, c := range []struct {
		useGateway bool
		want       string
	}{{true, "nous"}, {false, "fal"}} {
		cfg := config.Tree{"image_gen": map[string]any{"use_gateway": !c.useGateway}, "video_gen": map[string]any{"use_gateway": !c.useGateway}}
		if err := s.selectPluginImageGenProvider("fal", cfg, c.useGateway); err != nil {
			t.Fatal(err)
		}
		if err := s.selectPluginVideoGenProvider("fal", cfg, c.useGateway); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"image_gen", "video_gen"} {
			sec := section(cfg, key)
			if _, has := sec["use_gateway"]; sec["provider"] != c.want || has {
				t.Fatalf("%v %s %v", c.useGateway, key, sec)
			}
		}
	}
}

// Reconfigure run with the prints and model picker quiet and the Portal access gate green
func quietReconfigure(t *testing.T) *ToolsSetup {
	s := quietSelectors(t)
	swapHook(t, &ToolsConfigHooks.EnsureNousPortalAccess, func(context.Context, string, io.Writer, string, string) bool { return true })
	return s
}

func TestReconfigureManagedFalRowKeepsManagedSelection(t *testing.T) {
	s := quietReconfigure(t)
	row := caseRow("name", "Nous Subscription", "env_vars", []any{}, "requires_nous_auth", true, "managed_nous_feature", "image_gen",
		"override_env_vars", []any{"FAL_KEY"}, "imagegen_backend", "fal")
	cfg := config.Tree{"image_gen": map[string]any{"model": "fal-ai/gpt-image-2", "use_gateway": true}}
	if err := s.reconfigureProvider(row, cfg, true); err != nil {
		t.Fatal(err)
	}
	imageGen := section(cfg, "image_gen")
	if _, has := imageGen["use_gateway"]; imageGen["provider"] != "nous" || has {
		t.Fatalf("managed reconfigure %v", imageGen)
	}
}

func TestReconfigureDirectFalRowWritesVendorSelection(t *testing.T) {
	s := quietReconfigure(t)
	row := caseRow("name", "FAL.ai", "env_vars", []any{}, "imagegen_backend", "fal")
	cfg := config.Tree{"image_gen": map[string]any{"use_gateway": true}}
	if err := s.reconfigureProvider(row, cfg, true); err != nil {
		t.Fatal(err)
	}
	imageGen := section(cfg, "image_gen")
	if _, has := imageGen["use_gateway"]; imageGen["provider"] != "fal" || has {
		t.Fatalf("direct reconfigure %v", imageGen)
	}
}

func TestChromiumPostSetupInstallFailureReportsError(t *testing.T) {
	for _, failure := range []string{"pm", "worker"} {
		t.Run(failure, func(t *testing.T) {
			s, out := newTestToolsSetup(t, "")
			swapHook(t, &postSetupEnsureBrowserUseCLI, func(*ToolsSetup) {})
			swapHook(t, &postSetupIsTermux, func() bool { return false })
			absent := filepath.Join(t.TempDir(), "absent")
			swapHook(t, &ToolsConfigHooks.BakedChromiumFile, func() string { return absent })
			swapHook(t, &ToolsConfigHooks.ChromiumExecutable, func(*Env) string { return "" })
			var failure error = pm.NewInstallError("chromium", "fatal: network error", "")
			if t.Name() == "TestChromiumPostSetupInstallFailureReportsError/worker" {
				failure = &pm.PyError{Class: "RuntimeError", Message: "worker exited unsuccessfully"}
			}
			swapHook(t, &postSetupEnsure, func(context.Context, string, pm.EnsureOptions) error { return failure })
			if err := s.runPostSetup("chromium"); err != nil {
				t.Fatal(err)
			}
			if !anyContains(printedWith(out.String(), "⚠ "), failure.Error()) {
				t.Fatalf("warning lacks %q:\n%s", failure.Error(), out.String())
			}
			if !anyContains(printedWith(out.String(), "  "), "icebath tools post-setup chromium") {
				t.Fatalf("info lacks the retry command:\n%s", out.String())
			}
		})
	}
}

func TestBrowserExecEngineCheckedForEveryBrowserRowBrowserPostSetupAttemptsCliInstall(t *testing.T) {
	for _, key := range []string{"chromium", "browserbase", "lightpanda"} {
		t.Run(key, func(t *testing.T) {
			s, _ := newTestToolsSetup(t, "")
			ensured := 0
			swapHook(t, &postSetupEnsureBrowserUseCLI, func(*ToolsSetup) { ensured++ })
			swapHook(t, &postSetupWhich, func(string) string { return "" })
			swapHook(t, &postSetupEnsure, func(context.Context, string, pm.EnsureOptions) error { return nil })
			swapHook(t, &ToolsConfigHooks.ChromiumExecutable, func(*Env) string { return "/pm/chrome" })
			swapHook(t, &ToolsConfigHooks.LightpandaBinary, func(*Env) string { return "" })
			if err := s.runPostSetup(key); err != nil {
				t.Fatal(err)
			}
			if ensured != 1 {
				t.Fatalf("%s ensured %d times", key, ensured)
			}
		})
	}
}

func TestImagegenBackendRegistryImageGenProvidersTaggedWithRegisteredBackend(t *testing.T) {
	for _, p := range catProviders(categoryOf("image_gen")) {
		if _, ok := imagegenBackends[rowText(p, "imagegen_backend")]; !ok {
			t.Fatalf("%s missing a registered imagegen_backend tag", rowText(p, "name"))
		}
	}
}

func TestImagegenModelPickerPickerWritesChosenModelToConfig(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &toolsPromptChoice, func(*ToolsSetup, string, []string, int) (int, error) { return 1, nil })
	cfg := config.Tree{}
	if err := s.configureImagegenModel("fal", cfg); err != nil {
		t.Fatal(err)
	}
	catalog, def := imagegenBackends["fal"].catalogFn(s, config.Tree{})
	model := pyStr(section(cfg, "image_gen")["model"])
	if model == def || !catalog.has(model) {
		t.Fatalf("model %q default %q", model, def)
	}
}

func TestImagegenModelPickerPickerWithGptImageDoesNotPromptQuality(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	catalog, def := imagegenBackends["fal"].catalogFn(s, config.Tree{})
	ordered := []string{def}
	for _, id := range catalog.ids {
		if id != def {
			ordered = append(ordered, id)
		}
	}
	gptIdx := slices.Index(ordered, "fal-ai/gpt-image-1.5")
	calls := 0
	swapHook(t, &toolsPromptChoice, func(*ToolsSetup, string, []string, int) (int, error) { calls++; return gptIdx, nil })
	cfg := config.Tree{}
	if err := s.configureImagegenModel("fal", cfg); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("Expected 1 picker call (model only), got %d", calls)
	}
	imageGen := section(cfg, "image_gen")
	if _, has := imageGen["quality_setting"]; imageGen["model"] != "fal-ai/gpt-image-1.5" || has {
		t.Fatalf("image_gen %v", imageGen)
	}
}

func TestImagegenModelPickerPickerRepairsCorruptConfigSection(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &toolsPromptChoice, func(*ToolsSetup, string, []string, int) (int, error) { return 0, nil })
	cfg := config.Tree{"image_gen": "some-garbage-string"}
	if err := s.configureImagegenModel("fal", cfg); err != nil {
		t.Fatal(err)
	}
	_, def := imagegenBackends["fal"].catalogFn(s, config.Tree{})
	imageGen, ok := pyDictOf(cfg["image_gen"])
	if !ok || imageGen["model"] != def {
		t.Fatalf("image_gen %v", cfg["image_gen"])
	}
}

func TestImagegenModelPickerPluginPickerFallsBackWhenDefaultIsMissingFromCatalog(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.ImageGenRegistry, fakePickerRegistry(nil, &fakePickerProvider{name: "openrouter", available: true,
		models: []map[string]any{{"id": "openai/gpt-5.4-image-2", "strengths": "quality"}, {"id": "google/gemini-3-pro-image", "strengths": "fallback"}},
		def:    "also-missing"}))
	swapHook(t, &toolsPromptChoice, func(*ToolsSetup, string, []string, int) (int, error) { return 0, nil })
	cfg := config.Tree{"image_gen": map[string]any{"model": "gpt-image-2-medium"}}
	if err := s.configureImagegenModelForPlugin("openrouter", cfg); err != nil {
		t.Fatal(err)
	}
	if got := section(cfg, "image_gen")["model"]; got != "openai/gpt-5.4-image-2" {
		t.Fatalf("model %v", got)
	}
}

func TestVisibleProvidersReusesLoggedOutFeatureSnapshot(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.NousFeatures, func(context.Context, string, map[string]any, bool) NousToolFeatures {
		t.Fatal("feature snapshot was resolved again")
		return NousToolFeatures{}
	})
	features := &NousToolFeatures{Features: map[string]NousToolFeature{}, Account: accountDouble(false, nil, false)}
	rows, err := s.VisibleProviders(categoryOf("image_gen"), config.Tree{}, false, features)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rows, func(p *pyjson.Object) bool { return managedFeatureOf(p) == "image_gen" }) {
		t.Fatalf("managed image row hidden %v", rows)
	}
}

func TestVisibleProvidersReusesPoolVideoFeatureSnapshot(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.NousFeatures, func(context.Context, string, map[string]any, bool) NousToolFeatures {
		t.Fatal("feature snapshot was resolved again")
		return NousToolFeatures{}
	})
	features := &NousToolFeatures{NousAuthPresent: true, Features: map[string]NousToolFeature{}, Account: accountDouble(true, boolRef(false), true)}
	rows, err := s.VisibleProviders(categoryOf("video_gen"), config.Tree{}, false, features)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(rows, func(p *pyjson.Object) bool { return managedFeatureOf(p) == "video_gen" }) {
		t.Fatalf("managed video row offered to a pool only account %v", rows)
	}
}

func TestPluginProviderActiveOverridesManagedNousActiveLabel(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.NousFeatures, nousFeaturesDouble(false, map[string]NousToolFeature{"image_gen": {ManagedByNous: true}}, nil))
	cfg := config.Tree{"image_gen": map[string]any{"provider": "openai", "use_gateway": false}}
	nous := caseRow("name", "Nous Subscription", "managed_nous_feature", "image_gen")
	openai := caseRow("name", "OpenAI", "image_gen_plugin_name", "openai")
	if ok, err := s.IsProviderActive(openai, cfg, false); err != nil || !ok {
		t.Fatalf("openai %v %v", ok, err)
	}
	if ok, err := s.IsProviderActive(nous, cfg, false); err != nil || ok {
		t.Fatalf("nous %v %v", ok, err)
	}
}

func TestCodexOAuthBootstrapHookHookStartsCodexOauthOnlyWhenCredentialsMissing(t *testing.T) {
	for _, loggedIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "logged_out", true: "logged_in"}[loggedIn], func(t *testing.T) {
			s, _ := newTestToolsSetup(t, "")
			swapHook(t, &ToolsConfigHooks.CodexAuthStatus, func(context.Context, string) map[string]any { return map[string]any{"logged_in": loggedIn} })
			swapHook(t, &ToolsConfigHooks.SetupPromptChoice, func(string, []string, int) int { return 0 })
			started := 0
			var saved []bool
			swapHook(t, &ToolsConfigHooks.CodexDeviceCodeLogin, func(context.Context, string, io.Writer) (map[string]any, error) {
				started++
				return map[string]any{"tokens": map[string]any{"access_token": "t"}, "last_refresh": "x"}, nil
			})
			swapHook(t, &ToolsConfigHooks.SaveCodexTokens, func(_ context.Context, _ string, _ map[string]any, _ string, setActive bool) error {
				saved = append(saved, setActive)
				return nil
			})
			if err := postSetupHooks["openai_codex"](s); err != nil {
				t.Fatal(err)
			}
			wantStarted, wantSaved := 1, []bool{false}
			if loggedIn {
				wantStarted, wantSaved = 0, nil
			}
			if started != wantStarted || !slices.Equal(saved, wantSaved) {
				t.Fatalf("started %d saved %v", started, saved)
			}
		})
	}
}

func TestCodexOAuthBootstrapHookHookPrintsAuthCommandInsteadOfDeviceLoginWhenNoninteractive(t *testing.T) {
	s, out := newTestToolsSetup(t, "")
	t.Setenv("ICEBATH_NONINTERACTIVE", "1")
	swapHook(t, &ToolsConfigHooks.CodexAuthStatus, func(context.Context, string) map[string]any { return map[string]any{"logged_in": false} })
	swapHook(t, &ToolsConfigHooks.SetupPromptChoice, func(string, []string, int) int { return 0 })
	if err := postSetupHooks["openai_codex"](s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "icebath auth add openai-codex") {
		t.Fatalf("output %q", out.String())
	}
}

func TestCodexOAuthBootstrapHookReadinessReportsCodexRowFromAuthStore(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	row := caseRow("name", "OpenAI (Codex auth)", "env_vars", []any{}, "image_gen_plugin_name", "openai-codex", "post_setup", "openai_codex")
	for _, c := range []struct {
		loggedIn bool
		want     string
	}{{false, "needs_auth"}, {true, "ready"}} {
		swapHook(t, &ToolsConfigHooks.CodexAuthStatus, func(context.Context, string) map[string]any { return map[string]any{"logged_in": c.loggedIn} })
		if got, err := s.ProviderReadinessStatus(row, config.Tree{}, nil, nil); err != nil || got != c.want {
			t.Fatalf("logged_in %v: %q %v", c.loggedIn, got, err)
		}
	}
}

func TestLightpandaPostSetupReportsBinaryWhenFound(t *testing.T) {
	s, out := newTestToolsSetup(t, "")
	ensured := 0
	swapHook(t, &postSetupEnsureBrowserUseCLI, func(*ToolsSetup) { ensured++ })
	swapHook(t, &ToolsConfigHooks.LightpandaBinary, func(*Env) string { return "/opt/lightpanda" })
	if err := s.runPostSetup("lightpanda"); err != nil {
		t.Fatal(err)
	}
	success := printedWith(out.String(), "✓ ")
	if ensured != 1 || len(success) == 0 || !strings.Contains(success[len(success)-1], "/opt/lightpanda") {
		t.Fatalf("ensured %d output %q", ensured, out.String())
	}
	if warn := printedWith(out.String(), "⚠ "); len(warn) != 0 {
		t.Fatalf("warned %v", warn)
	}
}

func TestLightpandaPostSetupPrintsInstallHintWhenMissing(t *testing.T) {
	s, out := newTestToolsSetup(t, "")
	swapHook(t, &postSetupEnsureBrowserUseCLI, func(*ToolsSetup) {})
	swapHook(t, &ToolsConfigHooks.LightpandaBinary, func(*Env) string { return "" })
	if err := s.runPostSetup("lightpanda"); err != nil {
		t.Fatal(err)
	}
	warn := printedWith(out.String(), "⚠ ")
	if len(warn) == 0 || !strings.Contains(warn[len(warn)-1], "not found") {
		t.Fatalf("warnings %v", warn)
	}
	if !anyContains(printedWith(out.String(), "  "), "https://lightpanda.io/docs/run-locally/installation/one-liner") {
		t.Fatalf("info lacks the install url:\n%s", out.String())
	}
}

func TestLightpandaPostSetupPostSetupKeyIsValidAndReadinessGated(t *testing.T) {
	s, _ := newTestToolsSetup(t, "")
	if !s.ValidPostSetupKeys()["lightpanda"] {
		t.Fatal("lightpanda is not a valid post-setup key")
	}
	ready := postSetupReady["lightpanda"]
	swapHook(t, &ToolsConfigHooks.LightpandaBinary, func(*Env) string { return "/opt/lightpanda" })
	if ok, err := ready(s); err != nil || !ok {
		t.Fatalf("found %v %v", ok, err)
	}
	swapHook(t, &ToolsConfigHooks.LightpandaBinary, func(*Env) string { return "" })
	if ok, err := ready(s); err != nil || ok {
		t.Fatalf("missing %v %v", ok, err)
	}
	if _, gated := postSetupInstalled["lightpanda"]; gated {
		t.Fatal("lightpanda forces setup")
	}
}
