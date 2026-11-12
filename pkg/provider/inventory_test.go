package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Fresh home with every inventory seam restored after the test and no staged local models
func invTestHome(t *testing.T) string {
	t.Helper()
	h := newTestHome(t)
	savedList, savedPool, savedPricing, savedTier, savedCached := inventoryListProvidersFn, inventoryLoadPoolFn, inventoryPricingFn, inventoryCheckNousTierFn, inventoryCachedNousTierFn
	savedUsage, savedStaged, savedState, savedExplicit, savedOAuth := inventoryNousPickerUsageFn, inventoryStagedModelsFn, inventoryStateEndpointFn, inventoryExplicitFn, inventoryAnthropicOAuthFn
	savedMeta, savedPartition, savedWarmOR, savedORCaps, savedWarmNous, savedNousCaps := inventoryMetadataConfigFn, inventoryPartitionFn, inventoryWarmOpenRouterFn, inventoryOpenRouterCapsFn, inventoryWarmNousCapsFn, inventoryNousCapsFn
	savedCaps, savedFeatured, savedStrip := inventoryApplyCapsFn, inventoryApplyFeaturedFn, inventoryStripOverlapsFn
	t.Cleanup(func() {
		inventoryApplyCapsFn, inventoryApplyFeaturedFn, inventoryStripOverlapsFn = savedCaps, savedFeatured, savedStrip
		inventoryListProvidersFn, inventoryLoadPoolFn, inventoryPricingFn, inventoryCheckNousTierFn, inventoryCachedNousTierFn = savedList, savedPool, savedPricing, savedTier, savedCached
		inventoryNousPickerUsageFn, inventoryStagedModelsFn, inventoryStateEndpointFn, inventoryExplicitFn, inventoryAnthropicOAuthFn = savedUsage, savedStaged, savedState, savedExplicit, savedOAuth
		inventoryMetadataConfigFn, inventoryPartitionFn, inventoryWarmOpenRouterFn, inventoryOpenRouterCapsFn, inventoryWarmNousCapsFn, inventoryNousCapsFn = savedMeta, savedPartition, savedWarmOR, savedORCaps, savedWarmNous, savedNousCaps
		config.ResetCache()
	})
	inventoryStagedModelsFn = func() []string { return nil }
	inventoryWarmOpenRouterFn = func(context.Context) {}
	inventoryWarmNousCapsFn = func(context.Context) {}
	config.ResetCache()
	return h
}

// Replaces the authenticated provider listing with fixed rows
func invListReturning(rows []map[string]any) {
	inventoryListProvidersFn = func(context.Context, AuthenticatedProvidersOptions) ([]map[string]any, error) { return rows, nil }
}

func invRow(slug, name string, models []any, current, userDefined bool, source string) map[string]any {
	return map[string]any{"slug": slug, "name": name, "models": models, "total_models": len(models), "is_current": current,
		"is_user_defined": userDefined, "source": source}
}

func invCtx(provider, model, baseURL string) PickerConfigContext {
	return PickerConfigContext{CurrentProvider: provider, CurrentModel: model, CurrentBaseURL: baseURL,
		UserProviders: map[string]any{}, CustomProviders: []any{}}
}

func invPayload(t *testing.T, pc PickerConfigContext, opts ModelsPayloadOptions) []map[string]any {
	t.Helper()
	opts.ProbeCustomProviders = true
	payload, err := buildModelsPayload(context.Background(), pc, opts)
	if err != nil {
		t.Fatal(err)
	}
	return payloadRows(payload)
}

func invSlugs(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, pyStr(r["slug"]))
	}
	return out
}

func invFind(t *testing.T, rows []map[string]any, slug string) map[string]any {
	t.Helper()
	for _, r := range rows {
		if r["slug"] == slug {
			return r
		}
	}
	t.Fatalf("row %s missing from %v", slug, invSlugs(rows))
	return nil
}

func TestLoadPickerContextCoercesNumericYAMLProvider(t *testing.T) {
	h := invTestHome(t)
	writeConfigYAML(t, h, "model:\n  provider: 2070\n  default: Qwen3.5-9B-Q4_K_M.gguf\n  base_url: http://192.168.1.10:8082/v1\n"+
		"providers:\n  2070:\n    name: 2070\n    base_url: http://192.168.1.10:8082/v1\n    model: Qwen3.5-9B-Q4_K_M.gguf\ncustom_providers: []\n")
	pc, err := loadPickerContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(pc.UserProviders))
	for k := range pc.UserProviders {
		keys = append(keys, k)
	}
	if pc.CurrentProvider != "2070" || !reflect.DeepEqual(keys, []string{"2070"}) || pc.CurrentModel != "Qwen3.5-9B-Q4_K_M.gguf" ||
		pc.CurrentBaseURL != "http://192.168.1.10:8082/v1" {
		t.Fatalf("context %+v", pc)
	}
}

func TestBuildModelsPayloadReturnsExpectedShape(t *testing.T) {
	h := invTestHome(t)
	writeConfigYAML(t, h, "moa:\n  presets:\n    default:\n      enabled: true\n")
	rows := []map[string]any{invRow("openrouter", "OpenRouter", []any{"m1"}, true, false, "built-in")}
	want := []map[string]any{invRow("openrouter", "OpenRouter", []any{"m1"}, true, false, "built-in")}
	invListReturning(rows)
	payload, err := buildModelsPayload(context.Background(), invCtx("openrouter", "m1", ""), ModelsPayloadOptions{ProbeCustomProviders: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 3 || payload["model"] != "m1" || payload["provider"] != "openrouter" {
		t.Fatalf("payload %v", payload)
	}
	got := payloadRows(payload)
	if got[0]["slug"] != "moa" || !reflect.DeepEqual(got[0]["models"], []any{"default"}) || !reflect.DeepEqual(got[1:], want) {
		t.Fatalf("rows %v", got)
	}
}

func TestBuildModelsPayloadHidesMoAWithoutRawPreset(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{invRow("openrouter", "OpenRouter", []any{"m1"}, true, false, "built-in")})
	if got := invSlugs(invPayload(t, invCtx("openrouter", "m1", ""), ModelsPayloadOptions{})); !reflect.DeepEqual(got, []string{"openrouter"}) {
		t.Fatalf("slugs %v", got)
	}
}

func TestIncludeUnconfiguredAppendsCanonicalSkeletons(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{invRow("openrouter", "OpenRouter", []any{"m1"}, true, false, "built-in")})
	rows := invPayload(t, invCtx("openrouter", "orig-model", "orig-url"), ModelsPayloadOptions{IncludeUnconfigured: true})
	seen := map[string]bool{}
	for _, s := range invSlugs(rows) {
		seen[s] = true
	}
	for _, entry := range ListedCanonicalProviders() {
		if entry.Slug != "moa" && !seen[entry.Slug] {
			t.Fatalf("missing %s", entry.Slug)
		}
	}
	if seen["solstice"] {
		t.Fatal("pre-release provider listed")
	}
	for _, r := range rows {
		if r["source"] == "canonical" && (!reflect.DeepEqual(r["models"], []any{}) || r["total_models"] != 0) {
			t.Fatalf("skeleton %v", r)
		}
	}
}

func TestExplicitOnlyFiltersAmbientCredentialsButKeepsCurrentAndCustomRows(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{
		invRow("openai-codex", "OpenAI Codex", []any{"gpt-5.4"}, true, false, "hermes"),
		invRow("gemini", "Gemini", []any{"gemini-2.5-pro"}, false, false, "built-in"),
		invRow("copilot", "Copilot", []any{"gpt-5.4"}, false, false, "hermes"),
		invRow("nous", "Nous", []any{"anthropic/claude-sonnet-5"}, false, false, "hermes"),
		invRow("custom:lab", "Lab", []any{"lab-1"}, false, true, "user-config"),
		invRow("moa", "MoA", []any{"default"}, false, false, "virtual"),
	})
	inventoryExplicitFn = func(_ PoolEnv, slug string) bool { return slug == "gemini" }
	got := invSlugs(invPayload(t, invCtx("openai-codex", "gpt-5.4", "orig-url"), ModelsPayloadOptions{ExplicitOnly: true}))
	if !reflect.DeepEqual(got, []string{"openai-codex", "gemini", "custom:lab"}) {
		t.Fatalf("slugs %v", got)
	}
}

func TestExplicitOnlyKeepsAnthropicRowWithOAuthCredentials(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{
		invRow("anthropic", "Anthropic", []any{"claude-sonnet-5"}, false, false, "hermes"),
		invRow("copilot", "Copilot", []any{"gpt-5.4"}, false, false, "hermes"),
	})
	inventoryExplicitFn = func(PoolEnv, string) bool { return false }
	inventoryAnthropicOAuthFn = func(context.Context) bool { return true }
	got := invSlugs(invPayload(t, invCtx("opencode-go", "glm-5.3", "orig-url"), ModelsPayloadOptions{ExplicitOnly: true}))
	if !slices.Contains(got, "anthropic") {
		t.Fatalf("Anthropic OAuth login must survive the explicit-only filter, slugs %v", got)
	}
	if slices.Contains(got, "copilot") {
		t.Fatalf("ambient credential discovery must stay filtered, slugs %v", got)
	}
}

func TestExplicitOnlyDropsAnthropicRowWithoutOAuthCredentials(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{
		invRow("anthropic", "Anthropic", []any{"claude-sonnet-5"}, false, false, "hermes"),
	})
	inventoryExplicitFn = func(PoolEnv, string) bool { return false }
	inventoryAnthropicOAuthFn = func(context.Context) bool { return false }
	got := invSlugs(invPayload(t, invCtx("opencode-go", "glm-5.3", "orig-url"), ModelsPayloadOptions{ExplicitOnly: true}))
	if slices.Contains(got, "anthropic") {
		t.Fatalf("slugs %v", got)
	}
}

func TestAnthropicOAuthPresenceAcceptsPoolOnlyOAuthEntry(t *testing.T) {
	h := invTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"anthropic": []any{
		map[string]any{"id": "a1", "auth_type": "oauth", "access_token": "sk-ant-oat01-pool"}}}})
	if !anthropicOAuthCredentialsPresent(context.Background()) {
		t.Fatal("pool oauth entry not accepted")
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"anthropic": []any{
		map[string]any{"id": "a1", "auth_type": "api_key", "access_token": "sk-ant-api03-key"}}}})
	if anthropicOAuthCredentialsPresent(context.Background()) {
		t.Fatal("api key pool entry counted as oauth")
	}
}

func TestPickerHintsMarksAuthedRowsAuthenticated(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{invRow("openrouter", "OpenRouter", []any{"m1"}, true, false, "built-in")})
	if rows := invPayload(t, invCtx("orig", "orig-model", "orig-url"), ModelsPayloadOptions{PickerHints: true}); rows[0]["authenticated"] != true {
		t.Fatalf("row %v", rows[0])
	}
}

func TestPickerHintsAPIKeyWarningFormat(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{})
	rows := invPayload(t, invCtx("orig", "orig-model", "orig-url"), ModelsPayloadOptions{IncludeUnconfigured: true, PickerHints: true})
	if w := pyStr(invFind(t, rows, "anthropic")["warning"]); !strings.Contains(w, "ANTHROPIC_API_KEY") {
		t.Fatalf("warning %q", w)
	}
}

func TestCanonicalOrderUsesSlugNotIsUserDefinedFlag(t *testing.T) {
	invTestHome(t)
	canonicalSlug := CanonicalProviders()[2].Slug
	invListReturning([]map[string]any{
		invRow("custom:Ollama", "Ollama", []any{}, false, true, "user-config"),
		invRow(canonicalSlug, "x", []any{"m1"}, false, true, "built-in"),
	})
	got := invSlugs(invPayload(t, invCtx("orig", "orig-model", "orig-url"), ModelsPayloadOptions{CanonicalOrder: true}))
	if slices.Index(got, canonicalSlug) > slices.Index(got, "custom:Ollama") {
		t.Fatalf("canonical %s demoted %v", canonicalSlug, got)
	}
}

func TestCanonicalOrderWithUnconfiguredPreservesFullUniverse(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{invRow("custom:Ollama", "Ollama", []any{}, false, true, "user-config")})
	got := invSlugs(invPayload(t, invCtx("orig", "orig-model", "orig-url"), ModelsPayloadOptions{IncludeUnconfigured: true, PickerHints: true, CanonicalOrder: true}))
	visible := 0
	for _, e := range ListedCanonicalProviders() {
		if e.Slug != "moa" {
			visible++
		}
	}
	if got[0] != CanonicalProviders()[0].Slug || slices.Index(got, "custom:Ollama") < visible {
		t.Fatalf("order %v", got)
	}
}

func TestEndToEndWithRealContextNoCredentialsLeak(t *testing.T) {
	pickerTestHome(t, nil)
	h := invTestHome(t)
	canary := "sk-canary-XYZ-must-not-appear"
	t.Setenv("OPENROUTER_API_KEY", canary)
	t.Setenv("ANTHROPIC_API_KEY", canary)
	writeConfigYAML(t, h, "model:\n  provider: openrouter\nproviders: {}\ncustom_providers: []\n")
	pc, err := loadPickerContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := buildModelsPayload(context.Background(), pc, ModelsPayloadOptions{IncludeUnconfigured: true, PickerHints: true, ProbeCustomProviders: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(payload)
	if strings.Contains(string(raw), canary) {
		t.Fatal("credential leaked into the payload")
	}
}

func TestPayloadShapeCompatibleWithModelPickerDialogFrontend(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{invRow("openrouter", "OpenRouter", []any{"m1"}, true, false, "built-in")})
	for _, row := range invPayload(t, invCtx("orig", "orig-model", "orig-url"), ModelsPayloadOptions{IncludeUnconfigured: true, PickerHints: true}) {
		for _, k := range []string{"name", "slug", "models", "total_models", "is_current", "authenticated"} {
			if _, ok := row[k]; !ok {
				t.Fatalf("row %v missing %s", row["slug"], k)
			}
		}
	}
}

func TestUserDefinedRowsCarryAliasSetForGUICurrentMatch(t *testing.T) {
	invTestHome(t)
	myep := invRow("myep", "My Endpoint", []any{"my-model"}, true, true, "user-config")
	myep["api_url"] = "http://localhost:8000/v1"
	invListReturning([]map[string]any{myep, invRow("nous", "Nous", []any{"openai/gpt-5.5"}, false, false, "built-in")})
	rows := invPayload(t, invCtx("custom:myep", "my-model", "orig-url"), ModelsPayloadOptions{})
	aliases, _ := invFind(t, rows, "myep")["aliases"].([]any)
	for _, want := range []string{"custom:myep", "myep", "custom:my-endpoint"} {
		if !slices.Contains(aliases, any(want)) {
			t.Fatalf("aliases %v lack %s", aliases, want)
		}
	}
	if _, has := invFind(t, rows, "nous")["aliases"]; has {
		t.Fatal("built-in row carries aliases")
	}
}

func TestAggregatorDedupRemovesOverlappingModels(t *testing.T) {
	invTestHome(t)
	invListReturning([]map[string]any{
		invRow("litellm-proxy", "Litellm-Proxy", []any{"nvidia/nim/minimax-m3", "nvidia/nim/kimi-k2.6"}, false, true, "user-config"),
		invRow("openrouter", "Openrouter", []any{"minimax/minimax-m3", "nvidia/nim/minimax-m3", "anthropic/claude-sonnet-4.6"}, false, false, "built-in"),
	})
	rows := invPayload(t, invCtx("orig", "orig-model", "orig-url"), ModelsPayloadOptions{})
	or, proxy := invFind(t, rows, "openrouter"), invFind(t, rows, "litellm-proxy")
	if !reflect.DeepEqual(proxy["models"], []any{"nvidia/nim/minimax-m3", "nvidia/nim/kimi-k2.6"}) ||
		!reflect.DeepEqual(or["models"], []any{"minimax/minimax-m3", "anthropic/claude-sonnet-4.6"}) || or["total_models"] != 2 {
		t.Fatalf("openrouter %v proxy %v", or, proxy)
	}
}

func TestFlatNamespaceResellerKeepsFirstPartyModelsOverlappingUserProxy(t *testing.T) {
	invTestHome(t)
	goModels := []any{"kimi-k2.6", "minimax-m3", "minimax-m2.7", "glm-5", "deepseek-v4-flash", "qwen3.7-max"}
	invListReturning([]map[string]any{
		invRow("custom:my-proxy", "Custom:My-Proxy", []any{"minimax-m3", "minimax-m2.7", "glm-5", "deepseek-v4-flash"}, false, true, "user-config"),
		invRow("opencode-go", "Opencode-Go", append([]any(nil), goModels...), false, false, "built-in"),
		invRow("openrouter", "Openrouter", []any{"minimax-m3", "anthropic/claude-sonnet-4.6"}, false, false, "built-in"),
	})
	rows := invPayload(t, invCtx("orig", "orig-model", "orig-url"), ModelsPayloadOptions{})
	goRow, or := invFind(t, rows, "opencode-go"), invFind(t, rows, "openrouter")
	if !reflect.DeepEqual(goRow["models"], goModels) || goRow["total_models"] != 6 ||
		slices.Contains(or["models"].([]any), any("minimax-m3")) || !slices.Contains(or["models"].([]any), any("anthropic/claude-sonnet-4.6")) {
		t.Fatalf("opencode-go %v openrouter %v", goRow, or)
	}
}

func TestBuildModelsPayloadNoMaxModelsReturnsFullList(t *testing.T) {
	invTestHome(t)
	full := []any{}
	for i := range 100 {
		full = append(full, "model-"+pyStr(i))
	}
	invListReturning([]map[string]any{invRow("kilocode", "Kilo Code", append([]any(nil), full...), false, false, "built-in")})
	kilo := invFind(t, invPayload(t, invCtx("orig", "orig-model", "orig-url"), ModelsPayloadOptions{}), "kilocode")
	if !reflect.DeepEqual(kilo["models"], full) || kilo["total_models"] != 100 {
		t.Fatalf("kilocode %v", kilo["total_models"])
	}
}

func TestListAuthenticatedProvidersRefreshBustsCache(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	path := providerModelsCachePath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListAuthenticatedProviders(ctx, AuthenticatedProvidersOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("refresh false cleared the provider models cache")
	}
	if _, err := ListAuthenticatedProviders(ctx, AuthenticatedProvidersOptions{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("refresh true left the provider models cache")
	}
}

func TestPickerMetadataUsesOneConfigReadForRealModelsDevLookups(t *testing.T) {
	h := invTestHome(t)
	writeConfigYAML(t, h, "providers:\n  lab:\n    catalog_provider: openrouter\nmodel_overrides: {}\n")
	models := []string{"openai/model-a", "anthropic/model-b", "openai/model-c", "anthropic/model-d", "openai/model-e",
		"anthropic/model-f", "openai/model-g", "anthropic/model-h"}
	registry := map[string]any{}
	for i, m := range models {
		registry[m] = map[string]any{"id": m, "tool_call": true, "reasoning": strings.HasPrefix(m, "openai/"),
			"release_date": "2026-01-0" + pyStr(i+1), "limit": map[string]any{"context": 200000.0, "output": 8192.0}}
	}
	savedCache, savedTime := modelsDevCache, modelsDevCacheTime
	modelsDevStateMu.Lock()
	modelsDevCache, modelsDevCacheTime = map[string]any{"openrouter": map[string]any{"models": registry}}, time.Now().Add(time.Hour)
	modelsDevStateMu.Unlock()
	t.Cleanup(func() {
		modelsDevStateMu.Lock()
		modelsDevCache, modelsDevCacheTime = savedCache, savedTime
		modelsDevStateMu.Unlock()
	})
	ctx := context.Background()
	snapshot, err := inventoryMetadataConfigFn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		capsBase, okB := ModelsDevCapabilities(ctx, "custom:lab", m, false)
		capsSnap, okS := modelsDevCapabilitiesIn(h, "custom:lab", m, false, snapshot)
		infoBase, okIB := ModelsDevModelInfo(ctx, "custom:lab", m, false)
		infoSnap, okIS := modelsDevModelInfoIn(h, "custom:lab", m, false, snapshot)
		if okB != okS || okIB != okIS || !reflect.DeepEqual(capsBase, capsSnap) || !reflect.DeepEqual(infoBase, infoSnap) {
			t.Fatalf("snapshot lookup differs for %s", m)
		}
	}
	realMeta := inventoryMetadataConfigFn
	build := func(ids []string) (map[string]any, int) {
		reads := 0
		inventoryMetadataConfigFn = func(ctx context.Context) (config.Tree, error) { reads++; return realMeta(ctx) }
		invListReturning([]map[string]any{invRow("custom:lab", "Lab", stringsToAny(ids), false, true, "user-config")})
		rows := invPayload(t, invCtx("orig", "orig-model", "orig-url"), ModelsPayloadOptions{Capabilities: true, Featured: true})
		return rows[0], reads
	}
	small, smallReads := build(models[:3])
	large, largeReads := build(models)
	if smallReads != 1 || largeReads != 1 {
		t.Fatalf("config reads %d %d", smallReads, largeReads)
	}
	want := map[string]any{}
	for _, m := range models[:3] {
		want[m] = map[string]any{"fast": false, "reasoning": strings.HasPrefix(m, "openai/")}
	}
	if !reflect.DeepEqual(small["capabilities"], want) || !reflect.DeepEqual(large["featured_models"], []any{}) {
		t.Fatalf("capabilities %v featured %v", small["capabilities"], large["featured_models"])
	}
}

type invFakePool struct{ state *LimitState }

func (p invFakePool) Entries() []PooledCredential            { return nil }
func (p invFakePool) LimitState(models []string) *LimitState { return p.state }

func TestApplyLimitsSkipsWindowPlanRows(t *testing.T) {
	h := invTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{
		"nous": []any{map[string]any{"id": "n1"}}, "example": []any{map[string]any{"id": "e1"}}}})
	inventoryLoadPoolFn = func(context.Context, string) inventoryPool {
		return invFakePool{state: &LimitState{Scope: "account", ResetsAt: 1_800_000_000}}
	}
	rows := []map[string]any{
		{"slug": "nous", "models": []any{"a/b"}, "plan_usage": map[string]any{"kind": "usage_window"}},
		{"slug": "example", "models": []any{"a/b"}},
	}
	if err := applyLimits(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	limit, _ := rows[1]["limit"].(map[string]any)
	if _, has := rows[0]["limit"]; has || limit["scope"] != "account" || limit["resets_at"] != "2027-01-15T08:00:00+00:00" {
		t.Fatalf("rows %v", rows)
	}
}

var invAuxConfig = `model:
  provider: openrouter
  default: anthropic/claude-opus-4.6
model_catalog:
  excluded_providers: [copilot]
providers:
  my-llm:
    name: My LLM
    base_url: https://myllm.example.com/v1
    key_env: MYLLM_KEY
    discover_models: false
    models:
      big-model: {}
      small-model: {}
custom_providers:
  - name: Legacy Box
    base_url: https://legacy.example.com/v1
    key_env: LEGACY_KEY
    model: legacy-1
    discover_models: false
`

// Home with one providers entry and one legacy custom_providers entry, both keyed through env
func invAuxHome(t *testing.T) {
	t.Helper()
	pickerTestHome(t, nil)
	h := invTestHome(t)
	writeConfigYAML(t, h, invAuxConfig)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	t.Setenv("MYLLM_KEY", "sk-mine")
	t.Setenv("LEGACY_KEY", "sk-legacy")
}

func TestAuxPickerSurfacesUserDefinedProviders(t *testing.T) {
	invAuxHome(t)
	rows, err := BuildAuxPickerRows(context.Background(), "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	slugs := invSlugs(rows)
	if !slices.Contains(slugs, "my-llm") || !slices.Contains(slugs, "custom:legacy-box") {
		t.Fatalf("slugs %v", slugs)
	}
}

func TestAuxPickerRequestsExhaustedPoolVisibility(t *testing.T) {
	invAuxHome(t)
	var seen AuthenticatedProvidersOptions
	inventoryListProvidersFn = func(_ context.Context, opts AuthenticatedProvidersOptions) ([]map[string]any, error) {
		seen = opts
		return nil, nil
	}
	if _, err := BuildAuxPickerRows(context.Background(), "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if !seen.ForPicker {
		t.Fatal("aux picker did not request for_picker visibility")
	}
}
