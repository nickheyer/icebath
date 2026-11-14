package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestIsArceeTrinityThinkingMatches(t *testing.T) {
	for _, model := range []string{"trinity-large-thinking", "arcee-ai/trinity-large-thinking", "Arcee-AI/Trinity-Large-Thinking", "  trinity-large-thinking  "} {
		if !auxIsArceeTrinityThinking(model) {
			t.Errorf("%q not matched", model)
		}
	}
}

func TestCompressionThresholdDefaultNoneForOtherModels(t *testing.T) {
	for _, model := range []string{"", "", "trinity-large-preview", "claude-sonnet-4.6", "kimi-k2"} {
		if v, ok := auxCompressionThresholdForModel(model, "", true); ok {
			t.Errorf("%q gave %v", model, v)
		}
	}
}

func TestIsCodexGPT54OrGPT55RejectsNon5455Models(t *testing.T) {
	for _, model := range []string{"gpt-5", "gpt-5.55", "gpt-5.50", "gpt-5.45", "gpt-5.40", "gpt-daybreak-blue-latest-mini", "", ""} {
		if auxIsCodexGPT54OrGPT55(model, "openai-codex") {
			t.Errorf("%q matched", model)
		}
	}
}

func TestCompressionThresholdForCodexGPT55(t *testing.T) {
	for _, model := range []string{"gpt-5.4", "gpt-5.4-pro", "openai/gpt-5.4", "gpt-5.5", "gpt-5.5-pro", "openai/gpt-5.5", "gpt-daybreak-blue-latest"} {
		if _, ok := auxCompressionThresholdForModel(model, "openai-codex", true); !ok {
			t.Errorf("%q has no threshold", model)
		}
	}
	if !auxIsCodexGPT54OrGPT55("gpt-daybreak-blue-latest", "openai-codex") {
		t.Fatal("daybreak alias not matched")
	}
}

func Test900kVariantsKeepGlobalThreshold(t *testing.T) {
	for _, model := range []string{"gpt-5.6-sol-900k", "gpt-5.6-terra-900k", "gpt-5.6-luna-900k", "gpt-5.4-900k", "gpt-daybreak-blue-latest-900k", "openai/gpt-5.6-sol-900k"} {
		if auxIsCodexGPT54OrGPT55(model, "openai-codex") {
			t.Errorf("%q matched", model)
		}
		if v, ok := auxCompressionThresholdForModel(model, "openai-codex", true); ok {
			t.Errorf("%q gave %v", model, v)
		}
	}
}

func TestBaseSlugsStillAutoraisedAlongside900kVariants(t *testing.T) {
	if _, ok := auxCompressionThresholdForModel("gpt-5.6-sol", "openai-codex", true); !ok {
		t.Fatal("base slug lost its autoraise")
	}
	if _, ok := auxCompressionThresholdForModel("gpt-5.6-sol-900k", "openai-codex", true); ok {
		t.Fatal("900k variant autoraised")
	}
}

func TestIsCodexSparkRejectsNonSparkModels(t *testing.T) {
	for _, model := range []string{"gpt-5.5", "gpt-5.3-codex", "gpt-5.3", "gpt-5.3-codex-spark-mini", "", ""} {
		if auxIsCodexSpark(model, "openai-codex") {
			t.Errorf("%q matched", model)
		}
	}
}

func TestCompressionThresholdValuesAndGate(t *testing.T) {
	if v, _ := auxCompressionThresholdForModel("arcee-ai/trinity-large-thinking", "", true); v != 0.75 {
		t.Fatalf("arcee %v", v)
	}
	if v, _ := auxCompressionThresholdForModel("gpt-5.5", "openai-codex", true); v != 0.85 {
		t.Fatalf("codex %v", v)
	}
	if _, ok := auxCompressionThresholdForModel("gpt-5.5", "openai-codex", false); ok {
		t.Fatal("gated autoraise applied")
	}
	if v, _ := auxCompressionThresholdForModel("gpt-5.3-codex-spark", "openai-codex", false); v != 0.70 {
		t.Fatalf("spark %v", v)
	}
	if _, ok := auxCompressionThresholdForModel("gpt-5.5", "openai", true); ok {
		t.Fatal("non codex route autoraised")
	}
}

func TestFixedTemperatureForModelPolicyAndRejectionMemo(t *testing.T) {
	t.Cleanup(auxResetTemperatureRejections)
	cases := []struct {
		model, baseURL, provider string
		omit                     bool
		fixed                    float64
	}{
		{"moonshotai/kimi-k2", "", "", true, 0},
		{"gpt-5.4", "", "", true, 0},
		{"o3-mini", "", "", true, 0},
		{"gpt-5-chat-latest", "", "", false, 0},
		{"arcee-ai/trinity-large-thinking", "", "", false, 0.5},
		{"claude-sonnet-4.6", "", "", false, 0},
	}
	for _, c := range cases {
		omit, fixed, err := auxFixedTemperatureForModel(c.model, c.baseURL, c.provider)
		got := 0.0
		if fixed != nil {
			got = *fixed
		}
		if err != nil || omit != c.omit || got != c.fixed {
			t.Errorf("%q = %v %v %v", c.model, omit, got, err)
		}
	}
	if err := RememberTemperatureRejection("custom", "http://LocalHost:8080/v1", map[string]any{"model": "Org/Glm-4"}, nil); err != nil {
		t.Fatal(err)
	}
	if omit, _, _ := auxFixedTemperatureForModel("glm-4", "http://localhost:8080/v1", ""); !omit {
		t.Fatal("the remembered route and model must omit temperature on every path")
	}
	if omit, _, _ := auxFixedTemperatureForModel("glm-4", "http://localhost:9090/v1", ""); omit {
		t.Fatal("another port of the same host is another route")
	}
	if err := RememberTemperatureRejection("Zai", "", map[string]any{"model": "glm-5"}, nil); err != nil {
		t.Fatal(err)
	}
	if omit, _, _ := auxFixedTemperatureForModel("glm-5", "", "zai"); !omit {
		t.Fatal("provider keyed route without base URL")
	}
	if err := RememberTemperatureRejection("zai", "", map[string]any{"model": 5}, nil); err == nil {
		t.Fatal("a non string model raises AttributeError")
	}
	if _, _, err := auxFixedTemperatureForModel("glm-5", "http://[::1", ""); err == nil {
		t.Fatal("an invalid IPv6 base URL raises ValueError")
	}
	route := newCCRoute(Route{Provider: "custom", BaseURL: "http://localhost:8080/v1", Model: "glm-4"})
	if omit, _ := route.fixedTemperature(); !omit {
		t.Fatal("the chat completions path reads the same memo")
	}
}

func aliasHomes(t *testing.T) (pluginHome, bareHome string) {
	t.Helper()
	snapshotProviderRegistry(t)
	root := t.TempDir()
	pluginHome, bareHome = filepath.Join(root, "with_plugin"), filepath.Join(root, "bare")
	for _, h := range []string{pluginHome, bareHome} {
		if err := os.MkdirAll(h, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", bareHome)
	return pluginHome, bareHome
}

func aliasedFakeProfile() Profile {
	return Profile{Name: "aliased-fake", Aliases: []string{"aliasy"}, AuthType: AuthTypeAPIKey,
		KeyEnv: []string{"ALIASED_FAKE_KEY"}, BaseURL: "https://aliased.invalid/v1"}
}

func normalizeAuxIn(h, name string) string {
	expireHomeLayerChecks()
	return auxNormalizeAuxProvider(home.WithOverride(context.Background(), h), name)
}

func TestAliasTableIsPerHomeInEitherOrder(t *testing.T) {
	pluginHome, bareHome := aliasHomes(t)
	perHomePlugins(t, map[string]map[string]Profile{pluginHome: {"aliased-fake": aliasedFakeProfile()}})
	for i := 0; i < 2; i++ {
		if got := normalizeAuxIn(bareHome, "aliasy"); got != "aliasy" {
			t.Fatalf("bare home %q", got)
		}
		if got := normalizeAuxIn(pluginHome, "aliasy"); got != "aliased-fake" {
			t.Fatalf("plugin home %q", got)
		}
	}
}

func TestPluginInstalledWhileRunningReachesTheAliasTable(t *testing.T) {
	_, bareHome := aliasHomes(t)
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){
		"aliased-fake": func(string) ([]Profile, error) { return []Profile{aliasedFakeProfile()}, nil },
	})
	if got := normalizeAuxIn(bareHome, "aliasy"); got != "aliasy" {
		t.Fatalf("before install %q", got)
	}
	writeProviderPluginDir(t, bareHome, "aliased-fake")
	expireHomeLayerChecks()
	if got := normalizeAuxIn(bareHome, "aliasy"); got != "aliased-fake" {
		t.Fatalf("after install %q", got)
	}
}

func TestNormalizeAuxProviderPrefixesAndCodex(t *testing.T) {
	ctx := contextWithTempHome(t)
	for in, want := range map[string]string{"": "auto", "  ": "", "custom:": "custom", "custom: Ollama ": "custom", "CODEX": "openai-codex",
		"custom:codex": "openai-codex", "glm": "zai", "llama.cpp": "custom", "OpenRouter": "openrouter"} {
		if got := auxNormalizeAuxProvider(ctx, in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}

func fakeFastCatalog(t *testing.T, catalog PricingTable) *[]PricingFetchOptions {
	t.Helper()
	var calls []PricingFetchOptions
	old := auxFetchModelsWithPricingFn
	auxFetchModelsWithPricingFn = func(_ context.Context, opts PricingFetchOptions) PricingTable {
		calls = append(calls, opts)
		return catalog
	}
	t.Cleanup(func() { auxFetchModelsWithPricingFn = old })
	return &calls
}

func fastCatalogIDs(names ...string) PricingTable {
	out := PricingTable{}
	for _, n := range names {
		out[n] = map[string]any{}
	}
	return out
}

func TestCatalogMatchPrefersRollingAliasOverPinnedID(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeFastCatalog(t, fastCatalogIDs("z-ai/glm-5.2", "openai/gpt-5.4-mini", "~openai/gpt-mini-latest", "stepfun/step-3.7-flash:free"))
	if got := auxFastModelFromCatalog(ctx, "nous"); got != "~openai/gpt-mini-latest" {
		t.Fatalf("got %q", got)
	}
}

func TestCatalogMatchSkipsReasoningBatchAndEmbeddingLookalikes(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeFastCatalog(t, fastCatalogIDs("openai/o3-mini", "openai/gpt-5.4-mini:batch", "sentence-transformers/all-minilm-l6-v2", "google/gemini-3.6-flash"))
	if got := auxFastModelFromCatalog(ctx, "nous"); got != "google/gemini-3.6-flash" {
		t.Fatalf("got %q", got)
	}
}

func TestCatalogMatchSkipsTheNonChatSiblingsOfAChatModel(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeFastCatalog(t, fastCatalogIDs("openai/gpt-4o-mini-tts", "openai/gpt-4o-mini-transcribe", "openai/gpt-4o-mini-search-preview", "openai/gpt-4o-mini"))
	if got := auxFastModelFromCatalog(ctx, "nous"); got != "openai/gpt-4o-mini" {
		t.Fatalf("got %q", got)
	}
}

func TestCatalogMatchTakesTheNewestOfAFamily(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeFastCatalog(t, fastCatalogIDs("openai/gpt-3.5-mini", "openai/gpt-9-mini", "openai/gpt-10-mini"))
	if got := auxFastModelFromCatalog(ctx, "nous"); got != "openai/gpt-10-mini" {
		t.Fatalf("got %q", got)
	}
}

func TestCatalogFetchIsAuthenticated(t *testing.T) {
	ctx := contextWithTempHome(t)
	old := auxResolveAPIKeyProviderCredentialsFn
	auxResolveAPIKeyProviderCredentialsFn = func(context.Context, string) (apiKeyCreds, error) {
		return apiKeyCreds{APIKey: "sk-test", BaseURL: "https://api.example.com/v1"}, nil
	}
	t.Cleanup(func() { auxResolveAPIKeyProviderCredentialsFn = old })
	calls := fakeFastCatalog(t, PricingTable{})
	auxFastModelFromCatalog(ctx, "openai")
	if len(*calls) != 1 || (*calls)[0].APIKey != "sk-test" || (*calls)[0].BaseURL != "https://api.example.com" {
		t.Fatalf("calls %+v", *calls)
	}
}

func TestFallsBackToCuratedDefaultWhenCatalogUnavailable(t *testing.T) {
	ctx := contextWithTempHome(t)
	old := auxFastModelFromCatalogFn
	auxFastModelFromCatalogFn = func(context.Context, string) string { return "" }
	t.Cleanup(func() { auxFastModelFromCatalogFn = old })
	if fast, plain := auxAuxModelForProvider(ctx, "anthropic", true), auxGetAuxModelForProvider(ctx, "anthropic"); fast != plain || plain == "" {
		t.Fatalf("fast %q plain %q", fast, plain)
	}
}

func TestFastTierIsOptIn(t *testing.T) {
	ctx := contextWithTempHome(t)
	old := auxFastModelFromCatalogFn
	called := false
	auxFastModelFromCatalogFn = func(context.Context, string) string { called = true; return "" }
	t.Cleanup(func() { auxFastModelFromCatalogFn = old })
	auxGetAuxModelForProvider(ctx, "nous")
	if called {
		t.Fatal("the live catalog was read without prefer_fast")
	}
}

func TestOnlyTitlingIsInTheFastTier(t *testing.T) {
	if !auxFastModelTasks["title_generation"] {
		t.Fatal("titling missing")
	}
	for _, task := range []string{"compression", "vision", "web_extract"} {
		if auxFastModelTasks[task] {
			t.Errorf("%s in the fast tier", task)
		}
	}
}

func TestTaskPrefersFastModelReadsTheTaskConfig(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	write := func(text string) {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("auxiliary:\n  title_generation:\n    prefer_fast_model: 'yes'\n  compression:\n    prefer_fast_model: true\n")
	for task, want := range map[any]bool{"title_generation": true, "compression": false, nil: false, NewAuthoritativeAuxiliaryTask("title_generation"): true} {
		if got, err := auxTaskPrefersFastModel(ctx, task); err != nil || got != want {
			t.Errorf("%v = %v %v", task, got, err)
		}
	}
	write("auxiliary:\n  title_generation:\n    prefer_fast_model: 'off'\n")
	if got, _ := auxTaskPrefersFastModel(ctx, "title_generation"); got {
		t.Fatal("off is falsy")
	}
	scoped := SessionAuxiliaryScope(ctx, map[string]any{"title_generation": map[string]any{"prefer_fast_model": 1}})
	if got, _ := auxTaskPrefersFastModel(scoped, "title_generation"); !got {
		t.Fatal("the session scope wins over config.yaml")
	}
}

func TestModelRecencyKeyOrdersDigitRunsNumerically(t *testing.T) {
	order := []string{"gpt-3.5-mini", "gpt-5.4-mini", "gpt-9-mini", "gpt-10-mini"}
	for i := 1; i < len(order); i++ {
		if auxCompareRecency(auxModelRecencyKey(order[i-1]), auxModelRecencyKey(order[i])) >= 0 {
			t.Errorf("%s must sort before %s", order[i-1], order[i])
		}
	}
	want := []auxRecencyPart{{kind: 0, text: "gpt-"}, {kind: 1, num: 5.4}, {kind: 0, text: "-mini"}}
	if got := auxModelRecencyKey("GPT-5.4-mini"); !reflect.DeepEqual(got, want) {
		t.Fatalf("key %+v", got)
	}
	if got := auxModelRecencyKey("v٣"); len(got) != 2 || got[1].num != 3 {
		t.Fatalf("unicode digits %+v", got)
	}
}

func TestResolveProviderVisionDefault(t *testing.T) {
	ctx := contextWithTempHome(t)
	if got := auxResolveProviderVisionDefault(ctx, "zai"); got != "glm-5.3-flash" {
		t.Fatalf("zai %q", got)
	}
	if got := auxResolveProviderVisionDefault(ctx, "xiaomi"); got != "mimo-v2.5" {
		t.Fatalf("xiaomi %q", got)
	}
	if got := auxResolveProviderVisionDefault(ctx, "no-such-provider-xyz"); got != "" {
		t.Fatalf("unknown %q", got)
	}
	if !auxProvidersWithoutVision["kimi-coding"] || !auxProvidersWithoutVision["kimi-coding-cn"] || auxProvidersWithoutVision["zai"] {
		t.Fatal("providers without vision")
	}
}

func TestActualAliasesAndProfileMetadata(t *testing.T) {
	h := newTestHome(t)
	ctx := home.WithOverride(context.Background(), h)
	fakeModelsDevInfo(t, nil)
	profile, ok := GetProviderProfile(ctx, "actual-computer")
	if !ok || profile.Name != "actual" || profile.BaseURL != DefaultActualBaseURL || profile.Mode != ModeChatCompletions ||
		profile.AuthType != "api_key" || !reflect.DeepEqual(profile.KeyEnv, []string{"ACTUAL_API_KEY"}) {
		t.Fatalf("profile %+v", profile)
	}
	if got := NormalizeProviderDef(ctx, "aci"); got != "actual" {
		t.Fatalf("overlay %q", got)
	}
	if got := catalogNormalizeProvider("actualcomputer"); got != "actual" {
		t.Fatalf("models %q", got)
	}
	if got, err := resolveProvider(testEnv(t, h), "actual-computer"); err != nil || got != "actual" {
		t.Fatalf("auth %q %v", got, err)
	}
	if got := auxNormalizeAuxProvider(ctx, "aci"); got != "actual" {
		t.Fatalf("aux %q", got)
	}
	if got := determineModeCtx(ctx, "actual", "https://api.actual.inc", "", ""); got != ModeChatCompletions {
		t.Fatalf("api mode %q", got)
	}
}

func TestNormalizeAuxProviderWrapsUnexported(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	for _, name := range []string{"OpenRouter", " main ", "codex", ""} {
		if got, want := NormalizeAuxProvider(ctx, name), auxNormalizeAuxProvider(ctx, name); got != want {
			t.Fatalf("%q: got %q want %q", name, got, want)
		}
	}
}

// Temp home whose config routes the compression task to openrouter with a 45 second timeout
func auxCompressionTaskHome(t *testing.T) context.Context {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"compression": map[string]any{
		"provider": "openrouter", "model": "google/gemini-3-flash", "timeout": 45}}})
	return context.Background()
}

func TestCompressionThresholdExportsFollowPolicy(t *testing.T) {
	cases := []struct {
		model, provider string
		autoraise       bool
		want            float64
		ok              bool
	}{
		{"gpt-5.5", "openai-codex", true, 0.85, true},
		{"gpt-5.5", "openai-codex", false, 0, false},
		{"gpt-5.3-codex-spark", "openai-codex", false, 0.70, true},
		{"arcee-ai/trinity-large-thinking", "", false, 0.75, true},
		{"gpt-5.5", "openai", true, 0, false},
		{"claude-sonnet-4.6", "anthropic", true, 0, false},
	}
	for _, tc := range cases {
		got, ok := CompressionThresholdForModel(tc.model, tc.provider, tc.autoraise)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%s on %s autoraise %v gave %v %v, want %v %v", tc.model, tc.provider, tc.autoraise, got, ok, tc.want, tc.ok)
		}
	}
	if !IsCodexGPT54OrGPT55("gpt-5.4-mini", "openai-codex") || IsCodexGPT54OrGPT55("gpt-5.4", "openai") || IsCodexGPT54OrGPT55("gpt-5.3", "openai-codex") {
		t.Fatal("the 272K Codex family gate must match only gpt-5.4 to 5.6 families on the Codex OAuth route")
	}
	if !IsCodexSpark("gpt-5.3-codex-spark", " OpenAI-Codex ") || IsCodexSpark("gpt-5.3-codex", "openai-codex") || IsCodexSpark("gpt-5.3-codex-spark", "openrouter") {
		t.Fatal("the spark gate must match gpt-5.3-codex-spark on the Codex OAuth route only")
	}
}

func TestAuxiliaryTaskConfigExportReadsTheTaskBlock(t *testing.T) {
	cfg, err := AuxiliaryTaskConfig(auxCompressionTaskHome(t), "compression")
	if err != nil {
		t.Fatal(err)
	}
	if cfg["provider"] != "openrouter" || cfg["model"] != "google/gemini-3-flash" {
		t.Fatalf("task config %v", cfg)
	}
}

func TestExportedFixedTemperatureAndPortalWire(t *testing.T) {
	if omit, _, err := FixedTemperatureForModel("kimi-k2", "", ""); err != nil || !omit {
		t.Fatal("kimi omits temperature")
	}
	if ReasoningOfConfig(nil) != nil || ReasoningOfConfig(map[string]any{"effort": "high"}).Effort != "high" {
		t.Fatal("reasoning of config")
	}
	if s, ok := ValidatedOpenRouterProviderSort(" Price "); !ok || s != "price" {
		t.Fatal("sort")
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	if NousAPIMode(ctx, "anthropic/claude") != "chat_completions" || ParseConfigAPIMode(ctx, "responses") != "codex_responses" || GetNamedCustomProvider(ctx, "nope") != nil {
		t.Fatal("portal wire, api mode or named provider")
	}
	if CarrierRecord(ctx, map[string]any{}) != nil {
		t.Fatal("empty carrier record")
	}
	if _, ok := AnthropicLiveBlocks(nil); ok {
		t.Fatal("nil message blocks")
	}
}

func TestAuxModelForProviderReadsTheFastTierOnlyWhenPreferred(t *testing.T) {
	ctx := contextWithTempHome(t)
	old := auxFastModelFromCatalogFn
	auxFastModelFromCatalogFn = func(context.Context, string) string { return "fast-tier-model" }
	t.Cleanup(func() { auxFastModelFromCatalogFn = old })
	if got := AuxModelForProvider(ctx, "anthropic", true); got != "fast-tier-model" {
		t.Fatalf("prefer fast got %q", got)
	}
	if got := AuxModelForProvider(ctx, "anthropic", false); got != auxGetAuxModelForProvider(ctx, "anthropic") || got == "fast-tier-model" {
		t.Fatalf("plain got %q", got)
	}
}
