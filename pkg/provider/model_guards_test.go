package provider

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func guardTestHome(t *testing.T) context.Context {
	t.Helper()
	ctx := modelsTestHome(t)
	prevInfo, prevPricing, prevGuards, prevMeta := guardModelInfoFn, guardPricingEntryFn, selectionGuards, fetchEndpointModelMetadataFn
	t.Cleanup(func() {
		guardModelInfoFn, guardPricingEntryFn, selectionGuards, fetchEndpointModelMetadataFn = prevInfo, prevPricing, prevGuards, prevMeta
	})
	return ctx
}

func noModelInfo(ctx context.Context, provider, model string, allowNetwork bool) (ModelsDevEntry, bool) {
	return ModelsDevEntry{}, false
}

func ratIs(r *big.Rat, text string) bool {
	want, _ := new(big.Rat).SetString(text)
	return r != nil && r.Cmp(want) == 0
}

func TestModelGuardCostNoWarningAtThreshold(t *testing.T) {
	ctx := guardTestHome(t)
	info := &ModelsDevEntry{ID: "edge/model", Name: "edge/model", ProviderID: "anthropic", CostInput: 20.0, CostOutput: 100.0}
	if w := ExpensiveModelWarningFor(ctx, "edge/model", "anthropic", "", "", info); w != nil {
		t.Fatalf("warning at threshold: %+v", w)
	}
}

func TestModelGuardCostWarnsAboveInputThreshold(t *testing.T) {
	ctx := guardTestHome(t)
	info := &ModelsDevEntry{ID: "expensive/input", Name: "expensive/input", ProviderID: "anthropic", CostInput: 20.01, CostOutput: 1.0}
	w := ExpensiveModelWarningFor(ctx, "expensive/input", "anthropic", "", "", info)
	if w == nil || !ratIs(w.InputCostPerMillion, "20.01") {
		t.Fatalf("warning %+v", w)
	}
}

func TestModelGuardCostSkipsForeignModelsDevPricing(t *testing.T) {
	ctx := guardTestHome(t)
	for _, provider := range []string{"custom", "custom:routerai", "routerai"} {
		info := &ModelsDevEntry{ID: "vendor/priced-model", Name: "vendor/priced-model", ProviderID: "openrouter", CostInput: 25.0, CostOutput: 125.0}
		if w := ExpensiveModelWarningFor(ctx, "vendor/priced-model", provider, "", "", info); w != nil {
			t.Fatalf("%s: %+v", provider, w)
		}
	}
}

func TestModelGuardCostSkipsUntrustedPricingLookupForCustom(t *testing.T) {
	ctx := guardTestHome(t)
	guardModelInfoFn = noModelInfo
	calls := 0
	guardPricingEntryFn = func(ctx context.Context, modelName, provider, baseURL, apiKey string) (PricingEntry, bool) {
		calls++
		return PricingEntry{InputCostPerMillion: big.NewRat(25, 1), OutputCostPerMillion: big.NewRat(125, 1), Source: "provider_models_api"}, true
	}
	if w := ExpensiveModelWarningFor(ctx, "vendor/priced-model", "custom:routerai", "https://routerai.example/v1", "", nil); w != nil {
		t.Fatalf("warning %+v", w)
	}
	if calls != 0 {
		t.Fatalf("pricing lookup ran %d times", calls)
	}
}

func TestModelGuardCostKnownConfusingModelWarnsOnCustom(t *testing.T) {
	ctx := guardTestHome(t)
	w := ExpensiveModelWarningFor(ctx, "openai/gpt-5.5-pro", "custom:routerai", "https://routerai.example/v1", "", nil)
	if w == nil || !strings.Contains(w.Message, "did you mean to select openai/gpt-5.5?") {
		t.Fatalf("warning %+v", w)
	}
}

func TestModelGuardCostPricingEntryOutputAboveThreshold(t *testing.T) {
	ctx := guardTestHome(t)
	guardModelInfoFn = noModelInfo
	guardPricingEntryFn = func(ctx context.Context, modelName, provider, baseURL, apiKey string) (PricingEntry, bool) {
		in, _ := new(big.Rat).SetString("1.00")
		out, _ := new(big.Rat).SetString("100.01")
		return PricingEntry{InputCostPerMillion: in, OutputCostPerMillion: out, Source: "provider_models_api"}, true
	}
	w := ExpensiveModelWarningFor(ctx, "provider/expensive-output", "openrouter", "", "", nil)
	if w == nil || !ratIs(w.OutputCostPerMillion, "100.01") || !strings.Contains(w.Message, "$100.01/M") {
		t.Fatalf("warning %+v", w)
	}
}

func TestModelGuardCostGPT55ProWarnsWithoutPricing(t *testing.T) {
	ctx := guardTestHome(t)
	guardModelInfoFn = noModelInfo
	guardPricingEntryFn = func(ctx context.Context, modelName, provider, baseURL, apiKey string) (PricingEntry, bool) {
		return PricingEntry{}, false
	}
	w := ExpensiveModelWarningFor(ctx, "openai/gpt-5.5-pro", "openai-codex", "", "", nil)
	if w == nil || w.InputCostPerMillion != nil || w.OutputCostPerMillion != nil || !strings.Contains(w.Message, "did you mean to select openai/gpt-5.5?") {
		t.Fatalf("warning %+v", w)
	}
}

func TestModelGuardCostGPT55ProWarnsForNousPortalPricing(t *testing.T) {
	ctx := guardTestHome(t)
	guardModelInfoFn = noModelInfo
	fetchEndpointModelMetadataFn = func(ctx context.Context, baseURL, apiKey string, forceRefresh bool) map[string]map[string]any {
		return map[string]map[string]any{"openai/gpt-5.5-pro": {"pricing": map[string]any{"prompt": "0.000025", "completion": "0.000125"}}}
	}
	w := ExpensiveModelWarningFor(ctx, "openai/gpt-5.5-pro", "nous", "", "", nil)
	if w == nil || !ratIs(w.InputCostPerMillion, "25.000000") || !ratIs(w.OutputCostPerMillion, "125.000000") || !strings.Contains(w.Message, "did you mean to select openai/gpt-5.5?") {
		t.Fatalf("warning %+v", w)
	}
}

func TestModelGuardDataPolicy(t *testing.T) {
	w := DataTrainingWarningFor("muse-spark-1.2-contributor", "meta-ai")
	if w == nil || w.Model != "muse-spark-1.2-contributor" || w.Message == "" {
		t.Fatalf("contributor warning %+v", w)
	}
	for _, m := range []string{"muse-spark-1.2", "muse-spark-1.1"} {
		if DataTrainingWarningFor(m, "meta-ai") != nil {
			t.Fatalf("%s fired", m)
		}
	}
	for _, m := range []string{"anthropic/claude-opus-4.8", "gpt-5.6-sol", "deepseek-v4-pro", ""} {
		if DataTrainingWarningFor(m, "anthropic") != nil {
			t.Fatalf("%q fired", m)
		}
	}
	if DataTrainingWarningFor("muse-spark-1.2-contributor", "custom") == nil || DataTrainingWarningFor("muse-spark-1.2-contributor", "") == nil {
		t.Fatal("provider string gated the rule")
	}
	if DataTrainingWarningFor("MUSE-SPARK-1.2-CONTRIBUTOR", "meta-ai") == nil {
		t.Fatal("case sensitive match")
	}
}

func TestSelectionWarningRegistry(t *testing.T) {
	ctx := guardTestHome(t)
	if got := SelectionWarnings(ctx, SelectionRequest{Model: "some/ordinary-model"}); len(got) != 0 {
		t.Fatalf("ordinary model warned %+v", got)
	}
	if CombinedSelectionWarning(ctx, SelectionRequest{Model: "some/ordinary-model"}) != nil {
		t.Fatal("ordinary combined warning")
	}
	warnings := SelectionWarnings(ctx, SelectionRequest{Model: "muse-spark-1.2-contributor", Provider: "custom"})
	found := false
	for _, w := range warnings {
		if w.Kind == "data_policy" && strings.Contains(strings.ToLower(w.Message), "train") {
			found = true
		}
	}
	if !found {
		t.Fatalf("data policy missing %+v", warnings)
	}
	for _, w := range SelectionWarnings(ctx, SelectionRequest{Model: "muse-spark-1.2-contributor", Provider: "custom", IncludeKinds: []string{"cost"}}) {
		if w.Kind != "cost" {
			t.Fatalf("include kinds leaked %+v", w)
		}
	}
	if w := CombinedSelectionWarning(ctx, SelectionRequest{Model: "muse-spark-1.2-contributor"}); w == nil || w.Kind != "data_policy" {
		t.Fatalf("single combined %+v", w)
	}
	info := &ModelsDevEntry{ID: "pricey/model", Name: "pricey/model", ProviderID: "anthropic", CostInput: 50.0, CostOutput: 200.0}
	cost := false
	for _, w := range SelectionWarnings(ctx, SelectionRequest{Model: "pricey/model", Provider: "anthropic", ModelInfo: info}) {
		cost = cost || w.Kind == "cost"
	}
	if !cost {
		t.Fatal("cost guard silent through the registry")
	}
}

func TestSelectionWarningCombinedMergesMultiple(t *testing.T) {
	ctx := guardTestHome(t)
	costW := SelectionWarning{Kind: "cost", Title: "Expensive Model Warning", Model: "m", Provider: "p", Message: "COST BLOCK"}
	policyW := SelectionWarning{Kind: "data_policy", Title: "Data-Training Tier Warning", Model: "m", Provider: "p", Message: "POLICY BLOCK"}
	selectionGuards = []selectionGuard{
		func(context.Context, SelectionRequest) *SelectionWarning { return &costW },
		func(context.Context, SelectionRequest) *SelectionWarning { return &policyW },
	}
	merged := CombinedSelectionWarning(ctx, SelectionRequest{Model: "m"})
	if merged == nil || merged.Kind != "multiple" || !strings.Contains(merged.Message, "COST BLOCK") || !strings.Contains(merged.Message, "POLICY BLOCK") {
		t.Fatalf("merged %+v", merged)
	}
	if combinedSelectionMessage([]SelectionWarning{costW, policyW}) != "COST BLOCK\n\nPOLICY BLOCK" {
		t.Fatal("combined message join")
	}
}

func TestSelectionWarningMisbehavingGuardNeverBreaksSelection(t *testing.T) {
	ctx := guardTestHome(t)
	selectionGuards = []selectionGuard{func(context.Context, SelectionRequest) *SelectionWarning { panic("bad guard") }}
	if got := SelectionWarnings(ctx, SelectionRequest{Model: "anything"}); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func writeGuardConfig(t *testing.T, ctx context.Context, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestModelGuardContextCache(t *testing.T) {
	ctx := guardTestHome(t)
	guard := func(model string, sc *SelectionContext) *SelectionWarning {
		return contextCacheGuard(ctx, SelectionRequest{Model: model, Provider: "openrouter", Context: sc})
	}
	if guard("new/model", nil) != nil || guard("new/model", &SelectionContext{ContextTokens: 5_000, CurrentModel: "old/model"}) != nil {
		t.Fatal("fired below threshold")
	}
	tokens := DefaultContextCacheSwitchThreshold + 1
	w := guard("new/model", &SelectionContext{ContextTokens: tokens, CurrentModel: "old/model"})
	if w == nil || w.Kind != "context_cache" || !strings.Contains(w.Message, "100,001") {
		t.Fatalf("above default %+v", w)
	}
	if guard("same/model", &SelectionContext{ContextTokens: DefaultContextCacheSwitchThreshold * 2, CurrentModel: "same/model"}) != nil {
		t.Fatal("same model reselect fired")
	}
	withCtx := SelectionWarnings(ctx, SelectionRequest{Model: "new/model", Provider: "openrouter", Context: &SelectionContext{ContextTokens: tokens, CurrentModel: "old/model"}})
	without := SelectionWarnings(ctx, SelectionRequest{Model: "new/model", Provider: "openrouter"})
	has := func(ws []SelectionWarning) bool {
		for _, w := range ws {
			if w.Kind == "context_cache" {
				return true
			}
		}
		return false
	}
	if !has(withCtx) || has(without) {
		t.Fatalf("registry threading %v %v", withCtx, without)
	}
	writeGuardConfig(t, ctx, "model:\n  switch_context_confirm_tokens: 10000\n")
	if guard("new/model", &SelectionContext{ContextTokens: 20_000, CurrentModel: "old/model"}) == nil {
		t.Fatal("override threshold silent")
	}
	writeGuardConfig(t, ctx, "model:\n  switch_context_confirm_tokens: 0\n")
	if guard("new/model", &SelectionContext{ContextTokens: 1_000_000_000, CurrentModel: "old/model"}) != nil {
		t.Fatal("zero did not disable")
	}
}

func TestSelectionWarningContextForAgent(t *testing.T) {
	sc := SelectionContextFor(123_456, "current/model")
	if sc == nil || sc.ContextTokens != 123_456 || sc.CurrentModel != "current/model" {
		t.Fatalf("measured %+v", sc)
	}
	if SelectionContextFor(0, "current/model") != nil || SelectionContextFor(-1, "current/model") != nil {
		t.Fatal("unmeasured context produced facts")
	}
}
