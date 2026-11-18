package provider

import (
	"context"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func rat(text string) *big.Rat { return mustRat(text) }

func ratEq(a *big.Rat, text string) bool { return a != nil && a.Cmp(rat(text)) == 0 }

type pricingFixture struct {
	ctx            context.Context
	home           string
	deepinfraItems []map[string]any
}

// Isolated home, cleared pricing and registry caches, no endpoint probes
func newPricingFixture(t *testing.T) *pricingFixture {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, tmp)
	ResetPricingCache()
	ResetModelsDevCache()
	prevEndpoint, prevMeta, prevGet, prevClock := fetchEndpointModelMetadataFn, fetchModelMetadataFn, pricingGetJSON, pricingClock
	fetchEndpointModelMetadataFn = func(context.Context, string, string, bool) map[string]map[string]any {
		return map[string]map[string]any{}
	}
	fetchModelMetadataFn = func(context.Context, bool) map[string]map[string]any { return map[string]map[string]any{} }
	pricingGetJSON = func(url string, headers map[string]string, timeout time.Duration, _ bool) map[string]any {
		t.Errorf("unexpected catalog fetch of %s", url)
		return nil
	}
	f := &pricingFixture{ctx: home.WithOverride(context.Background(), tmp), home: tmp}
	prevDIURL, prevDICached, prevDIFetch := deepinfraCatalogURLFn, deepinfraCatalogCachedFn, fetchDeepinfraModelsByTagFn
	deepinfraCatalogURLFn = func() (string, string) {
		return "https://api.deepinfra.com/models/list", "https://api.deepinfra.com/models/list"
	}
	deepinfraCatalogCachedFn = func(string) bool { return f.deepinfraItems != nil }
	fetchDeepinfraModelsByTagFn = func(string, time.Duration, bool) []map[string]any { return f.deepinfraItems }
	t.Cleanup(func() {
		fetchEndpointModelMetadataFn, fetchModelMetadataFn, pricingGetJSON, pricingClock = prevEndpoint, prevMeta, prevGet, prevClock
		deepinfraCatalogURLFn, deepinfraCatalogCachedFn, fetchDeepinfraModelsByTagFn = prevDIURL, prevDICached, prevDIFetch
		ResetPricingCache()
		ResetModelsDevCache()
	})
	return f
}

func TestAstraWholeRequestPriceTierIncludesCacheWrites(t *testing.T) {
	f := newPricingFixture(t)
	below := EstimateUsageCost(f.ctx, "gpt-6-astra", CanonicalUsage{InputTokens: 100_000, OutputTokens: 10_000, CacheReadTokens: 10_000, CacheWriteTokens: 10_000, RequestCount: 1}, "openai", "", "")
	above := EstimateUsageCost(f.ctx, "gpt-6-astra", CanonicalUsage{InputTokens: 100_000, OutputTokens: 10_000, CacheReadTokens: 100_000, CacheWriteTokens: 100_001, RequestCount: 1}, "openai", "", "")
	entry := officialDocsPricing[pricingKey{"openai", "gpt-6-astra"}]
	want := new(big.Rat)
	for _, part := range []struct {
		tokens int64
		rate   *big.Rat
	}{{100_000, entry.InputCostPerMillionAbove}, {10_000, entry.OutputCostPerMillionAbove}, {100_000, entry.CacheReadCostPerMillionAbove}, {100_001, entry.CacheWriteCostPerMillionAbove}} {
		want.Add(want, new(big.Rat).Mul(big.NewRat(part.tokens, 1), part.rate))
	}
	want.Quo(want, ratOneMillion)
	if above.AmountUSD.Cmp(want) != 0 || below.AmountUSD.Cmp(above.AmountUSD) >= 0 {
		t.Fatalf("above %s want %s below %s", above.AmountUSD, want, below.AmountUSD)
	}
}

func TestDirectFirstPartyRoutePricesModelsMissingFromSnapshot(t *testing.T) {
	f := newPricingFixture(t)
	modelsDevStateMu.Lock()
	modelsDevCache = jsonMap(`{"openai": {"models": {"gpt-5-nano": {"cost": {"input": 0.05, "output": 0.4, "cache_read": 0.005}}}},
	 "xai": {"models": {"grok-4.3": {"cost": {"input": 1.25, "output": 2.5, "cache_read": 0.2}}}}}`)
	modelsDevCacheTime = time.Now()
	modelsDevStateMu.Unlock()
	usage := CanonicalUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000, RequestCount: 1}
	cases := []struct {
		provider, baseURL, model, status, amount string
	}{
		{"openai-api", "https://api.openai.com/v1", "gpt-5-nano", "estimated", "0.455"},
		{"openai", "", "gpt-5-nano", "estimated", "0.455"},
		{"xai", "https://api.x.ai/v1", "grok-4.3", "estimated", "3.95"},
		{"xai", "https://grok-relay.example.com/v1", "grok-4.3", "unknown", ""},
		{"xai", "http://api.x.ai/v1", "grok-4.3", "unknown", ""},
		{"xai-oauth", "https://api.x.ai/v1", "grok-4.3", "unknown", ""},
		{"custom", "https://api.x.ai/v1", "grok-4.3", "unknown", ""},
	}
	for _, tc := range cases {
		cost := EstimateUsageCost(f.ctx, tc.model, usage, tc.provider, tc.baseURL, "")
		if cost.Status != tc.status {
			t.Fatalf("%s %s: status %s", tc.provider, tc.baseURL, cost.Status)
		}
		if tc.amount == "" && cost.AmountUSD != nil || tc.amount != "" && !ratEq(cost.AmountUSD, tc.amount) {
			t.Fatalf("%s %s: amount %v", tc.provider, tc.baseURL, cost.AmountUSD)
		}
	}
}

func TestNormalizeUsageReadsDeepseekNativeCacheHitTokens(t *testing.T) {
	n := NormalizeUsage(map[string]any{"prompt_tokens": 2000, "completion_tokens": 400, "prompt_cache_hit_tokens": 1500, "prompt_cache_miss_tokens": 500}, "deepseek", "chat_completions")
	if n.CacheReadTokens != 1500 || n.InputTokens != 500 || n.OutputTokens != 400 {
		t.Fatalf("%+v", n)
	}
}

func TestNormalizeUsageOpenAIReadsTopLevelAnthropicCacheFields(t *testing.T) {
	n := NormalizeUsage(map[string]any{"prompt_tokens": 1000, "completion_tokens": 200, "prompt_tokens_details": map[string]any{"cached_tokens": 500}, "cache_creation_input_tokens": 300}, "openrouter", "chat_completions")
	if n.CacheReadTokens != 500 || n.CacheWriteTokens != 300 || n.InputTokens != 200 || n.OutputTokens != 200 {
		t.Fatalf("%+v", n)
	}
}

func TestNormalizeUsageKimiAndFallbackOrder(t *testing.T) {
	n := NormalizeUsage(map[string]any{"prompt_tokens": 3000, "completion_tokens": 250, "cached_tokens": 1800}, "kimi", "chat_completions")
	if n.CacheReadTokens != 1800 || n.InputTokens != 1200 || n.OutputTokens != 250 {
		t.Fatalf("kimi %+v", n)
	}
	nested := NormalizeUsage(map[string]any{"prompt_tokens": 1000, "completion_tokens": 100, "prompt_tokens_details": map[string]any{"cached_tokens": 400}, "cached_tokens": 999}, "kimi", "chat_completions")
	if nested.CacheReadTokens != 400 {
		t.Fatalf("nested must win %+v", nested)
	}
	deepseek := NormalizeUsage(map[string]any{"prompt_tokens": 2000, "completion_tokens": 100, "prompt_cache_hit_tokens": 1500, "cached_tokens": 999}, "deepseek", "chat_completions")
	if deepseek.CacheReadTokens != 1500 {
		t.Fatalf("deepseek hit tokens must win %+v", deepseek)
	}
	plain := NormalizeUsage(map[string]any{"prompt_tokens": 500, "completion_tokens": 50}, "kimi", "chat_completions")
	if plain.CacheReadTokens != 0 || plain.InputTokens != 500 || plain.RequestCount != 1 {
		t.Fatalf("plain %+v", plain)
	}
	if empty := NormalizeUsage(nil, "", ""); empty.RequestCount != 1 || empty.TotalTokens() != 0 {
		t.Fatalf("empty %+v", empty)
	}
}

func TestNormalizeUsageCodexShapes(t *testing.T) {
	type typed struct {
		InputTokens        int            `json:"input_tokens"`
		OutputTokens       int            `json:"output_tokens"`
		InputTokensDetails map[string]int `json:"input_tokens_details"`
	}
	payload := map[string]any{"input_tokens": 100, "output_tokens": 20, "input_tokens_details": map[string]any{"cached_tokens": 60, "cache_creation_tokens": 10}}
	fromMap := NormalizeUsage(payload, "openai-codex", "codex_responses")
	fromStruct := NormalizeUsage(typed{100, 20, map[string]int{"cached_tokens": 60, "cache_creation_tokens": 10}}, "openai-codex", "codex_responses")
	if fromMap.InputTokens != 30 || fromMap.CacheReadTokens != 60 || fromMap.CacheWriteTokens != 10 || fromMap.OutputTokens != 20 {
		t.Fatalf("map %+v", fromMap)
	}
	if fromStruct.InputTokens != fromMap.InputTokens || fromStruct.CacheWriteTokens != fromMap.CacheWriteTokens {
		t.Fatalf("struct %+v", fromStruct)
	}
	write := NormalizeUsage(map[string]any{"input_tokens": 2006, "output_tokens": 400, "input_tokens_details": map[string]any{"cached_tokens": 1920, "cache_write_tokens": 50}}, "openai", "codex_responses")
	if write.CacheReadTokens != 1920 || write.CacheWriteTokens != 50 || write.InputTokens != 2006-1920-50 {
		t.Fatalf("cache_write_tokens %+v", write)
	}
	legacy := NormalizeUsage(map[string]any{"input_tokens": 1000, "output_tokens": 100, "input_tokens_details": map[string]any{"cached_tokens": 200, "cache_creation_tokens": 80}}, "openai", "codex_responses")
	if legacy.CacheWriteTokens != 80 {
		t.Fatalf("legacy %+v", legacy)
	}
	reasoning := NormalizeUsage(map[string]any{"input_tokens": 10, "output_tokens": 5, "output_tokens_details": map[string]any{"reasoning_tokens": 3}}, "openai", "codex_responses")
	if reasoning.ReasoningTokens != 3 {
		t.Fatalf("reasoning %+v", reasoning)
	}
}

func TestNormalizeUsageClampsCounters(t *testing.T) {
	negative := NormalizeUsage(map[string]any{"prompt_tokens": 100, "completion_tokens": -5, "prompt_tokens_details": map[string]any{"cached_tokens": -10, "cache_write_tokens": -20}, "completion_tokens_details": map[string]any{"reasoning_tokens": -3}}, "openrouter", "chat_completions")
	if negative.InputTokens != 100 || negative.OutputTokens != 0 || negative.CacheReadTokens != 0 || negative.CacheWriteTokens != 0 || negative.ReasoningTokens != 0 {
		t.Fatalf("negative %+v", negative)
	}
	inconsistent := NormalizeUsage(map[string]any{"prompt_tokens": 100, "completion_tokens": 10, "prompt_tokens_details": map[string]any{"cached_tokens": 80, "cache_creation_input_tokens": 50}}, "openrouter", "chat_completions")
	if inconsistent.InputTokens != 0 || inconsistent.PromptTokens() != 130 {
		t.Fatalf("inconsistent %+v", inconsistent)
	}
	anthropic := NormalizeUsage(map[string]any{"input_tokens": 55, "output_tokens": 7113, "cache_read_input_tokens": 1369379, "cache_creation_input_tokens": 42135}, "bedrock", "anthropic_messages")
	if anthropic.InputTokens != 55 || anthropic.CacheReadTokens != 1369379 || anthropic.CacheWriteTokens != 42135 {
		t.Fatalf("anthropic %+v", anthropic)
	}
	sum := anthropic.Add(inconsistent)
	if sum.RequestCount != 2 || sum.OutputTokens != 7123 || sum.RawUsage != nil {
		t.Fatalf("sum %+v", sum)
	}
}

func TestBundledPricingSkipsEndpointMetadata(t *testing.T) {
	f := newPricingFixture(t)
	fetchEndpointModelMetadataFn = func(context.Context, string, string, bool) map[string]map[string]any {
		t.Fatal("endpoint metadata should not be fetched")
		return nil
	}
	entry, ok := GetPricingEntry(f.ctx, "deepseek-chat", "deepseek", "https://api.deepseek.com/v1", "")
	if !ok || entry.Source != "official_docs_snapshot" {
		t.Fatalf("%+v %v", entry, ok)
	}
}

func TestUnknownModelFallsBackToEndpointMetadata(t *testing.T) {
	f := newPricingFixture(t)
	fetchEndpointModelMetadataFn = func(context.Context, string, string, bool) map[string]map[string]any {
		return map[string]map[string]any{"deepseek-future": {"pricing": map[string]any{"prompt": "0.000001", "completion": "0.000002"}}}
	}
	entry, ok := GetPricingEntry(f.ctx, "deepseek-future", "deepseek", "https://api.deepseek.com/v1", "")
	if !ok || entry.Source != "provider_models_api" || !ratEq(entry.InputCostPerMillion, "1") || !ratEq(entry.OutputCostPerMillion, "2") {
		t.Fatalf("%+v %v", entry, ok)
	}
}

func TestDeepseekDeprecatedAliasesPriceAsFlash(t *testing.T) {
	f := newPricingFixture(t)
	flash, ok := GetPricingEntry(f.ctx, "deepseek-flash", "deepseek", "", "")
	if !ok {
		t.Fatal("no flash entry")
	}
	for _, alias := range []string{"deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"} {
		entry, ok := GetPricingEntry(f.ctx, alias, "deepseek", "", "")
		if !ok || entry.InputCostPerMillion.Cmp(flash.InputCostPerMillion) != 0 || entry.OutputCostPerMillion.Cmp(flash.OutputCostPerMillion) != 0 || entry.CacheReadCostPerMillion.Cmp(flash.CacheReadCostPerMillion) != 0 {
			t.Fatalf("%s: %+v", alias, entry)
		}
	}
}

func TestBedrockClaudeRowsAllCarryCachePricing(t *testing.T) {
	found := false
	for key, entry := range officialDocsPricing {
		if key.provider != "bedrock" || !strings.Contains(key.model, "claude") {
			continue
		}
		found = true
		if entry.InputCostPerMillion == nil || entry.CacheReadCostPerMillion == nil || entry.CacheWriteCostPerMillion == nil {
			t.Fatalf("%v lacks cache pricing", key)
		}
		if entry.CacheReadCostPerMillion.Cmp(entry.InputCostPerMillion) >= 0 || entry.CacheWriteCostPerMillion.Cmp(entry.InputCostPerMillion) <= 0 {
			t.Fatalf("%v cache rates out of order", key)
		}
	}
	if !found {
		t.Fatal("no bedrock claude rows")
	}
}

func TestBedrockVersionedInferenceProfileResolvesToBarePricing(t *testing.T) {
	f := newPricingFixture(t)
	bare, ok := GetPricingEntry(f.ctx, "anthropic.claude-sonnet-4-6", "bedrock", "", "")
	if !ok {
		t.Fatal("bare row missing")
	}
	for _, model := range []string{"us.anthropic.claude-sonnet-4-6-20250514-v1:0", "global.anthropic.claude-sonnet-4-6-20250514-v1:0"} {
		scoped, ok := GetPricingEntry(f.ctx, model, "bedrock", "", "")
		if !ok || scoped.InputCostPerMillion.Cmp(bare.InputCostPerMillion) != 0 || scoped.CacheWriteCostPerMillion.Cmp(bare.CacheWriteCostPerMillion) != 0 {
			t.Fatalf("%s: %+v", model, scoped)
		}
	}
	usage := NormalizeUsage(map[string]any{"input_tokens": 55, "output_tokens": 7113, "cache_read_input_tokens": 1369379, "cache_creation_input_tokens": 42135}, "bedrock", "anthropic_messages")
	result := EstimateUsageCost(f.ctx, "us.anthropic.claude-opus-4-6", usage, "bedrock", "https://bedrock-runtime.us-east-1.amazonaws.com", "")
	if result.Status != "estimated" || result.AmountUSD == nil {
		t.Fatalf("%+v", result)
	}
	for _, bare := range []string{"claude-opus-5", "claude-opus-5-5"} {
		direct, _ := GetPricingEntry(f.ctx, bare, "anthropic", "", "")
		for _, scope := range []string{"", "us.", "global."} {
			scoped, ok := GetPricingEntry(f.ctx, scope+"anthropic."+bare, "bedrock", "", "")
			if !ok || scoped.Source != "official_docs_snapshot" || scoped.InputCostPerMillion.Cmp(direct.InputCostPerMillion) != 0 || scoped.CacheReadCostPerMillion.Cmp(direct.CacheReadCostPerMillion) != 0 {
				t.Fatalf("%s%s: %+v", scope, bare, scoped)
			}
		}
	}
	resetCuratedCatalog(t)
	claude := 0
	for _, model := range staticCatalogModels("bedrock") {
		if !strings.Contains(model, "anthropic.claude") {
			continue
		}
		claude++
		if entry, ok := GetPricingEntry(f.ctx, model, "bedrock", "", ""); !ok || entry.Source != "official_docs_snapshot" {
			t.Fatalf("%s unpriced", model)
		}
	}
	if claude == 0 {
		t.Fatal("no claude rows in the bedrock catalog")
	}
}

func TestFireworksRouterFastTierPricesDistinctly(t *testing.T) {
	f := newPricingFixture(t)
	standard, ok1 := GetPricingEntry(f.ctx, "accounts/fireworks/models/kimi-k2p6", "fireworks", "https://api.fireworks.ai/inference/v1", "")
	fast, ok2 := GetPricingEntry(f.ctx, "accounts/fireworks/routers/kimi-k2p6-fast", "fireworks", "https://api.fireworks.ai/inference/v1", "")
	if !ok1 || !ok2 || fast.InputCostPerMillion.Cmp(standard.InputCostPerMillion) <= 0 || fast.OutputCostPerMillion.Cmp(standard.OutputCostPerMillion) <= 0 {
		t.Fatalf("%+v %+v", standard, fast)
	}
}

func TestGoogleAndVertexRoutesShareOfficialPricingSnapshot(t *testing.T) {
	f := newPricingFixture(t)
	for _, route := range []BillingRoute{
		ResolveBillingRoute("model", "gemini", ""),
		ResolveBillingRoute("google/model", "vertex", ""),
		ResolveBillingRoute("google/model", "custom", "https://aiplatform.googleapis.com/v1/projects/example"),
	} {
		if route.Provider != "google" || route.BillingMode != "official_docs_snapshot" {
			t.Fatalf("%+v", route)
		}
	}
	vertex, _ := ProfileFor("vertex")
	result := EstimateUsageCost(f.ctx, vertex.DefaultAuxModel, CanonicalUsage{InputTokens: 100, OutputTokens: 100, CacheReadTokens: 100, RequestCount: 1}, vertex.Name, vertex.BaseURL, "")
	if result.Status != "estimated" || result.AmountUSD == nil || result.AmountUSD.Sign() <= 0 {
		t.Fatalf("%+v", result)
	}
	resetCuratedCatalog(t)
	curated := map[string]bool{}
	for _, row := range OpenRouterModels {
		curated[row.ID] = true
	}
	var flash []string
	for mid := range curated {
		if strings.HasPrefix(mid, "google/gemini-") && strings.HasSuffix(mid, "-flash") {
			flash = append(flash, mid)
		}
	}
	sort.Strings(flash)
	if len(flash) == 0 {
		t.Fatal("no curated flash models")
	}
	usage := CanonicalUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000, RequestCount: 1}
	for _, model := range flash {
		bare := strings.SplitN(model, "/", 2)[1]
		gemini := EstimateUsageCost(f.ctx, bare, usage, "gemini", "", "")
		vertexCost := EstimateUsageCost(f.ctx, model, usage, "vertex", "", "")
		if gemini.Status != "estimated" || gemini.Source != "official_docs_snapshot" || vertexCost.AmountUSD.Cmp(gemini.AmountUSD) != 0 {
			t.Fatalf("%s: %+v %+v", model, gemini, vertexCost)
		}
		if !containsString(staticCatalogModels("gemini"), bare) || !containsString(staticCatalogModels("vertex"), model) {
			t.Fatalf("%s missing from the direct pickers", model)
		}
	}
}

func TestFormatCostLabel(t *testing.T) {
	cases := map[string]string{"0": "$0.00", "0.004640": "~$0.0046", "0.01": "~$0.01", "1.23": "~$1.23", "0.00004": "~$<0.0001", "0.00005": "~$<0.0001", "0.00015": "~$0.0002", "2.345": "~$2.34"}
	for in, want := range cases {
		if got := FormatCostLabel(rat(in)); got != want {
			t.Fatalf("%s -> %q want %q", in, got, want)
		}
	}
	if FormatCostLabel(nil) != "$0.00" {
		t.Fatal("nil amount")
	}
}

func TestSubscriptionIncludedCostHasNote(t *testing.T) {
	f := newPricingFixture(t)
	result := EstimateUsageCost(f.ctx, "gpt-5.4-mini", CanonicalUsage{InputTokens: 1000, OutputTokens: 500, RequestCount: 1}, "openai-codex", "", "")
	if result.Status != "included" || result.AmountUSD.Sign() != 0 || len(result.Notes) == 0 || result.Label != "included" {
		t.Fatalf("%+v", result)
	}
	if entry, ok := GetPricingEntry(f.ctx, "gpt-5.4-mini", "openai-codex", "", ""); !ok || entry.PricingVersion != "included-route" {
		t.Fatalf("%+v", entry)
	}
}

func TestGeminiProTieredPricing(t *testing.T) {
	f := newPricingFixture(t)
	usage := func(in, out, read int) CanonicalUsage {
		return CanonicalUsage{InputTokens: in, OutputTokens: out, CacheReadTokens: read, RequestCount: 1}
	}
	check := func(model string, u CanonicalUsage, want string) {
		t.Helper()
		result := EstimateUsageCost(f.ctx, model, u, "google", "", "")
		if result.Status != "estimated" || !ratEq(result.AmountUSD, want) {
			t.Fatalf("%s %+v: %+v", model, u, result)
		}
	}
	check("gemini-3.1-pro", usage(100_000, 10_000, 0), "0.32")
	check("gemini-3.1-pro", usage(200_000, 10_000, 0), "0.52")
	check("gemini-3.1-pro", usage(250_000, 10_000, 0), "1.18")
	check("gemini-3.1-pro", usage(150_000, 0, 100_000), "0.64")
	check("gemini-3.1-pro-preview", usage(250_000, 10_000, 0), "1.18")
	check("gemini-2.5-pro", usage(250_000, 10_000, 0), "0.775")
	check("gemini-2.5-pro", usage(150_000, 0, 100_000), "0.3875")
	entry, ok := GetPricingEntry(f.ctx, "gemini-3.1-flash-lite", "google", "", "")
	if !ok || entry.HasTier {
		t.Fatalf("%+v", entry)
	}
	check("gemini-3.1-flash-lite", usage(250_000, 10_000, 0), "0.0775")
}

func anthropicUsage(speed string) CanonicalUsage {
	payload := map[string]any{"input_tokens": 100_000, "output_tokens": 10_000, "cache_read_input_tokens": 200_000, "cache_creation_input_tokens": 50_000}
	if speed != "" {
		payload["speed"] = speed
	}
	return NormalizeUsage(payload, "anthropic", "anthropic_messages")
}

func TestAnthropicFastModeResponsesPriceFromTheFastRateRow(t *testing.T) {
	f := newPricingFixture(t)
	if len(anthropicFastModePricing) != len(anthropicFastModeModels) {
		t.Fatalf("fast rows %v gate %v", anthropicFastModePricing, anthropicFastModeModels)
	}
	for model := range anthropicFastModeModels {
		standardEntry, ok := GetPricingEntry(f.ctx, model, "anthropic", "", "")
		if !ok {
			t.Fatalf("%s has no standard rate", model)
		}
		fast := EstimateUsageCost(f.ctx, model, anthropicUsage("fast"), "anthropic", "", "")
		standard := EstimateUsageCost(f.ctx, model, anthropicUsage(""), "anthropic", "", "")
		if fast.PricingVersion != anthropicFastModePricing[model].PricingVersion || standard.PricingVersion != standardEntry.PricingVersion || fast.AmountUSD.Cmp(standard.AmountUSD) <= 0 {
			t.Fatalf("%s fast %+v standard %+v", model, fast, standard)
		}
		if EstimateUsageCost(f.ctx, model, anthropicUsage("standard"), "anthropic", "", "").AmountUSD.Cmp(standard.AmountUSD) != 0 {
			t.Fatalf("%s standard speed must bill like no field", model)
		}
	}
	dotted := EstimateUsageCost(f.ctx, "anthropic/claude-opus-5.5", anthropicUsage("fast"), "anthropic", "", "")
	plain := EstimateUsageCost(f.ctx, "claude-opus-5-5", anthropicUsage("fast"), "anthropic", "", "")
	if dotted.AmountUSD.Cmp(plain.AmountUSD) != 0 {
		t.Fatal("dotted vendor prefixed id must price from the same fast row")
	}
	unknown := EstimateUsageCost(f.ctx, "claude-sonnet-4-6", anthropicUsage("fast"), "anthropic", "", "")
	if unknown.AmountUSD != nil || unknown.Status != "unknown" {
		t.Fatalf("%+v", unknown)
	}
	ultra := EstimateUsageCost(f.ctx, "gpt-6.1-sol", WithServedServiceTier(CanonicalUsage{InputTokens: 1_000_000, RequestCount: 1}, "Ultrafast"), "openai", "", "")
	if !ratEq(ultra.AmountUSD, "24.00") {
		t.Fatalf("ultrafast %+v", ultra)
	}
	noUltra := EstimateUsageCost(f.ctx, "gpt-5.5", WithServedServiceTier(CanonicalUsage{InputTokens: 1, RequestCount: 1}, "ultrafast"), "openai", "", "")
	if noUltra.Status != "unknown" {
		t.Fatalf("%+v", noUltra)
	}
}

func TestOpenRouterAndRequestCostPricing(t *testing.T) {
	f := newPricingFixture(t)
	fetchModelMetadataFn = func(context.Context, bool) map[string]map[string]any {
		return map[string]map[string]any{"vendor/model": {"pricing": map[string]any{"prompt": "0.000001", "completion": "0.000002", "request": "0.01", "input_cache_read": "0.0000005"}}}
	}
	result := EstimateUsageCost(f.ctx, "vendor/model", CanonicalUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000, RequestCount: 2}, "openrouter", "https://openrouter.ai/api/v1", "")
	if !ratEq(result.AmountUSD, "3.52") || result.Source != "provider_models_api" || len(result.Notes) != 1 {
		t.Fatalf("%+v", result)
	}
	if !HasKnownPricing(f.ctx, "vendor/model", "openrouter", "", "") || HasKnownPricing(f.ctx, "vendor/other", "openrouter", "", "") {
		t.Fatal("HasKnownPricing")
	}
	missing := EstimateUsageCost(f.ctx, "vendor/model", CanonicalUsage{CacheWriteTokens: 10, RequestCount: 1}, "openrouter", "", "")
	if missing.Status != "unknown" || missing.Notes[0] != "cache-write pricing unavailable for route" {
		t.Fatalf("%+v", missing)
	}
}

func TestUsageCostHook(t *testing.T) {
	f := newPricingFixture(t)
	RegisterUsageCostHook("acme", func(model string, usage CanonicalUsage) (CostResult, bool) {
		return CostResult{AmountUSD: rat("0.5"), Status: "actual", Source: "provider_cost_api", Label: "~$0.50"}, true
	})
	t.Cleanup(func() { RegisterUsageCostHook("acme", nil) })
	result := EstimateUsageCost(f.ctx, "acme-1", CanonicalUsage{RequestCount: 1}, "acme", "", "")
	if result.Status != "actual" || !ratEq(result.AmountUSD, "0.5") {
		t.Fatalf("%+v", result)
	}
}

func TestCompactFormatters(t *testing.T) {
	for in, want := range map[float64]string{30: "30s", 90: "2m", 3600: "1h", 3660: "1h 1m", 90000: "1.0d"} {
		if got := FormatDurationCompact(in); got != want {
			t.Fatalf("%v -> %q", in, got)
		}
	}
	for in, want := range map[int]string{999: "999", 1000: "1K", 1500: "1.5K", 12345: "12.3K", 123456: "123K", 2_500_000: "2.5M", -1500: "-1.5K", 3_000_000_000: "3B"} {
		if got := FormatTokenCountCompact(in); got != want {
			t.Fatalf("%d -> %q", in, got)
		}
	}
}

const pricingBase = "https://inference-api.example.com"

func servePricing(f *pricingFixture, fn func(auth string) []string) *[]string {
	requests := &[]string{}
	pricingGetJSON = func(url string, headers map[string]string, timeout time.Duration, _ bool) map[string]any {
		auth := headers["Authorization"]
		*requests = append(*requests, auth)
		var data []any
		for _, mid := range fn(auth) {
			data = append(data, map[string]any{"id": mid, "pricing": map[string]any{"prompt": "0.000002", "completion": "0.00001"}})
		}
		return map[string]any{"data": data}
	}
	return requests
}

func pricingIDs(table PricingTable) []string {
	out := make([]string, 0, len(table))
	for k := range table {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestOneTokenDoesNotReceiveAnotherTokensCatalog(t *testing.T) {
	f := newPricingFixture(t)
	requests := servePricing(f, func(auth string) []string {
		if auth == "Bearer tok-a" {
			return []string{"org-a/only"}
		}
		return []string{"org-b/only"}
	})
	a := FetchModelsWithPricing(context.Background(), PricingFetchOptions{APIKey: "tok-a", BaseURL: pricingBase})
	b := FetchModelsWithPricing(context.Background(), PricingFetchOptions{APIKey: "tok-b", BaseURL: pricingBase})
	if !reflect.DeepEqual(pricingIDs(a), []string{"org-a/only"}) || !reflect.DeepEqual(pricingIDs(b), []string{"org-b/only"}) || len(*requests) != 2 {
		t.Fatalf("%v %v %v", a, b, *requests)
	}
	if strings.Contains(pricingAuthFingerprint("sk-super-secret"), "sk-super-secret") {
		t.Fatal("credential in cache key")
	}
}

func TestAnonymousAndAuthenticatedReadsAreSeparate(t *testing.T) {
	f := newPricingFixture(t)
	requests := servePricing(f, func(auth string) []string {
		if auth != "" {
			return []string{"vendor/allowed"}
		}
		return []string{"vendor/allowed", "vendor/blocked"}
	})
	anon := FetchModelsWithPricing(context.Background(), PricingFetchOptions{BaseURL: pricingBase})
	authed := FetchModelsWithPricing(context.Background(), PricingFetchOptions{APIKey: "sk-test", BaseURL: pricingBase})
	if len(anon) != 2 || len(authed) != 1 || !reflect.DeepEqual(*requests, []string{"", "Bearer sk-test"}) {
		t.Fatalf("%v %v %v", anon, authed, *requests)
	}
	for _, key := range []string{"sk-test", ""} {
		before := len(*requests)
		first := FetchModelsWithPricing(context.Background(), PricingFetchOptions{APIKey: key, BaseURL: pricingBase})
		second := FetchModelsWithPricing(context.Background(), PricingFetchOptions{APIKey: key, BaseURL: pricingBase})
		if !reflect.DeepEqual(first, second) || len(*requests) != before {
			t.Fatal("repeated read missed the cache")
		}
	}
	FetchModelsWithPricing(context.Background(), PricingFetchOptions{APIKey: "sk-test", BaseURL: pricingBase, ForceRefresh: true})
	if len(*requests) != 3 {
		t.Fatalf("force refresh requests %d", len(*requests))
	}
	if got := FetchModelsWithPricing(context.Background(), PricingFetchOptions{BaseURL: pricingBase}); len(got) != 2 || len(*requests) != 3 {
		t.Fatal("anonymous entry did not survive the forced authenticated read")
	}
}

func TestPeekCachedPricing(t *testing.T) {
	f := newPricingFixture(t)
	if got := PeekCachedPricing(pricingBase); len(got) != 0 {
		t.Fatal("empty expected")
	}
	requests := servePricing(f, func(auth string) []string {
		if auth != "" {
			return []string{"vendor/allowed"}
		}
		return []string{"vendor/allowed", "vendor/blocked"}
	})
	PeekCachedPricing(pricingBase)
	if len(*requests) != 0 {
		t.Fatal("peek fetched")
	}
	FetchModelsWithPricing(context.Background(), PricingFetchOptions{BaseURL: pricingBase})
	if got := pricingIDs(PeekCachedPricing(pricingBase)); !reflect.DeepEqual(got, []string{"vendor/allowed", "vendor/blocked"}) {
		t.Fatalf("anonymous fallback %v", got)
	}
	FetchModelsWithPricing(context.Background(), PricingFetchOptions{APIKey: "sk-test", BaseURL: pricingBase})
	if got := pricingIDs(PeekCachedPricing(pricingBase + "/v1")); !reflect.DeepEqual(got, []string{"vendor/allowed"}) {
		t.Fatalf("authenticated preferred through /v1 %v", got)
	}
}

func TestPricingCacheWriteDropsCatalogsPastTheirDeadline(t *testing.T) {
	newPricingFixture(t)
	now := time.Unix(100, 0)
	pricingClock = func() time.Time { return now }
	cacheCatalog("https://n.example\x00auth:old", PricingTable{"m": {"prompt": "1"}}, 300*time.Second)
	cacheCatalog("https://static.example", PricingTable{"m": {"prompt": "1"}}, 0)
	now = time.Unix(500, 0)
	cacheCatalog("https://n.example\x00auth:live", PricingTable{"m": {"prompt": "2"}}, 300*time.Second)
	cacheCatalog("https://n.example\x00auth:new", PricingTable{"m": {"prompt": "3"}}, 300*time.Second)
	pricingCacheMu.Lock()
	keys := pricingIDs(PricingTable(nil))
	for k := range pricingCache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_, oldDeadline := pricingCacheRetryAfter["https://n.example\x00auth:old"]
	pricingCacheMu.Unlock()
	if !reflect.DeepEqual(keys, []string{"https://n.example\x00auth:live", "https://n.example\x00auth:new", "https://static.example"}) || oldDeadline {
		t.Fatalf("keys %q", keys)
	}
	if got := cachedCatalog("https://n.example\x00auth:live"); got["m"]["prompt"] != "2" {
		t.Fatalf("%v", got)
	}
	now = time.Unix(801, 0)
	if cachedCatalog("https://n.example\x00auth:live") != nil {
		t.Fatal("entry did not expire")
	}
}

func TestFailedCatalogFetchIsNotCachedForever(t *testing.T) {
	newPricingFixture(t)
	calls := 0
	pricingGetJSON = func(url string, headers map[string]string, timeout time.Duration, _ bool) map[string]any {
		calls++
		return nil
	}
	now := time.Now()
	pricingClock = func() time.Time { return now }
	if got := FetchModelsWithPricing(context.Background(), PricingFetchOptions{BaseURL: "https://example.test"}); len(got) != 0 {
		t.Fatal("expected empty")
	}
	FetchModelsWithPricing(context.Background(), PricingFetchOptions{BaseURL: "https://example.test"})
	if calls != 1 || !PricingFetchSuppressed("https://example.test/v1") {
		t.Fatalf("calls %d suppressed %v", calls, PricingFetchSuppressed("https://example.test"))
	}
	pricingClock = func() time.Time { return now.Add(failedCatalogTTL + time.Second) }
	if PricingFetchSuppressed("https://example.test") {
		t.Fatal("still suppressed after the window")
	}
	FetchModelsWithPricing(context.Background(), PricingFetchOptions{BaseURL: "https://example.test"})
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
	ResetPricingCache()
	calls = 0
	pricingClock = func() time.Time { return now }
	pricingGetJSON = func(url string, headers map[string]string, timeout time.Duration, _ bool) map[string]any {
		calls++
		return jsonMap(`{"data": [{"id": "a/b", "pricing": {"prompt": "1", "completion": "2"}}]}`)
	}
	FetchModelsWithPricing(context.Background(), PricingFetchOptions{BaseURL: "https://example.test"})
	pricingClock = func() time.Time { return now.Add(failedCatalogTTL + time.Second) }
	if _, ok := FetchModelsWithPricing(context.Background(), PricingFetchOptions{BaseURL: "https://example.test"})["a/b"]; !ok || calls != 1 {
		t.Fatalf("successful catalog re fetched: %d", calls)
	}
}

func TestProviderPricingFetchersAndScopes(t *testing.T) {
	f := newPricingFixture(t)
	t.Setenv("KILOCODE_BASE_URL", "https://kilo.example/api/gateway/v1")
	t.Setenv("KILOCODE_API_KEY", "kk")
	t.Setenv("NOVITA_API_KEY", "nk")
	t.Setenv("NOVITA_BASE_URL", "")
	if kiloPricingScope() != "https://kilo.example/api/gateway" || novitaPricingScope() != "https://api.novita.ai/openai/v1" || aiGatewayPricingScope() != "https://ai-gateway.vercel.sh/v1" {
		t.Fatalf("scopes %s %s", kiloPricingScope(), novitaPricingScope())
	}
	pricingGetJSON = func(url string, headers map[string]string, timeout time.Duration, _ bool) map[string]any {
		switch {
		case strings.HasPrefix(url, "https://kilo.example"):
			return jsonMap(`{"data": [
			 {"id": "kilo-auto/free", "isFree": true, "pricing": {"prompt": "0.000001", "completion": "0.000002", "input_cache_read": "0.0000001"}},
			 {"id": "varies", "pricing": {"prompt": "-1", "completion": "-1"}},
			 {"id": "paid", "pricing": {"prompt": "0.000001", "completion": "0.000002"}}]}`)
		case strings.HasPrefix(url, "https://api.novita.ai"):
			return jsonMap(`{"data": [{"id": "n/1", "input_token_price_per_m": 1000, "output_token_price_per_m": 2000}, {"id": "n/none"}]}`)
		case strings.HasPrefix(url, AIGatewayBaseURL):
			return jsonMap(`{"data": [{"id": "v/1", "pricing": {"input": "0.000001", "output": "0.000002", "input_cache_read": "0.0000005"}}]}`)
		}
		t.Fatalf("unexpected url %s", url)
		return nil
	}
	kilo := GetPricingForProvider(f.ctx, "kilocode", "", false, false)
	if !reflect.DeepEqual(kilo["kilo-auto/free"], map[string]any{"prompt": "0", "completion": "0"}) || kilo["varies"] != nil || kilo["paid"]["prompt"] != "0.000001" {
		t.Fatalf("kilo %v", kilo)
	}
	novita := GetPricingForProvider(f.ctx, "novita", "", false, false)
	if novita["n/1"]["prompt"] != "1.0000000000000001e-07" || novita["n/1"]["completion"] != "2.0000000000000002e-07" || novita["n/none"] != nil {
		t.Fatalf("novita %v", novita)
	}
	gateway := GetPricingForProvider(f.ctx, "vercel", "", false, false)
	if gateway["v/1"]["prompt"] != "0.000001" || gateway["v/1"]["input_cache_read"] != "0.0000005" {
		t.Fatalf("gateway %v", gateway)
	}
	if !reflect.DeepEqual(GetPricingForProvider(f.ctx, "kilocode", "", false, true), kilo) {
		t.Fatal("cached only read missed the warm kilo catalog")
	}
	if len(GetPricingForProvider(f.ctx, "zai", "", false, false)) != 0 {
		t.Fatal("provider without pricing produced rows")
	}
	modelsDevStateMu.Lock()
	modelsDevCache = jsonMap(`{"fireworks-ai": {"models": {"accounts/fireworks/models/kimi-k2p6": {"cost": {"input": 0.95, "output": 4, "cache_read": 0.16}}, "nocost": {}}}}`)
	modelsDevCacheTime = time.Now()
	modelsDevStateMu.Unlock()
	fireworks := GetPricingForProvider(f.ctx, "fireworks", "", false, false)
	if fireworks["accounts/fireworks/models/kimi-k2p6"]["input_cache_read"] != "1.6e-07" || fireworks["nocost"] != nil {
		t.Fatalf("fireworks %v", fireworks)
	}
	if PricingCacheScope(f.ctx, "kilocode", "") != "https://kilo.example/api/gateway" || PricingCacheScope(f.ctx, "zai", "") != "" {
		t.Fatal("cache scope")
	}
	f.deepinfraItems = []map[string]any{{"id": "d/1", "metadata": map[string]any{"pricing": map[string]any{"input_tokens": 0.5, "output_tokens": 1, "cache_read_tokens": 0.1}}}, {"id": "d/none", "metadata": map[string]any{}}}
	deepinfra := GetPricingForProvider(f.ctx, "deepinfra", "", false, true)
	if deepinfra["d/1"]["prompt"] != "5e-07" || deepinfra["d/1"]["input_cache_read"] != "1.0000000000000001e-07" || deepinfra["d/none"] != nil {
		t.Fatalf("deepinfra %v", deepinfra)
	}
}

func TestResolvePricingProviderTrustBoundary(t *testing.T) {
	if ResolvePricingProvider("glm", "") != "zai" || ResolvePricingProvider("", "") != "openrouter" {
		t.Fatal("alias normalization")
	}
	if ResolvePricingProvider("custom:relay", "") != "" {
		t.Fatal("custom entry without a base url must stay unproven")
	}
	if got := ResolvePricingProvider("custom:openrouter", "https://openrouter.ai/api/v1"); got != "openrouter" {
		t.Fatalf("official host %q", got)
	}
	for _, lookalike := range []string{"https://openrouter.ai.attacker.invalid/v1", "https://notopenrouter.ai/v1"} {
		if got := ResolvePricingProvider("custom:openrouter", lookalike); got != "" {
			t.Fatalf("%s accepted as %q", lookalike, got)
		}
	}
}

func TestFormatPricePerMtok(t *testing.T) {
	for in, want := range map[string]string{"x": "?", "0": "free", "0.000002": "$2.00", "0.0000000005": "$0.0005", "0.000000000012": "$0.000012"} {
		if got := FormatPricePerMtok(in); got != want {
			t.Fatalf("%s -> %q want %q", in, got, want)
		}
	}
}
