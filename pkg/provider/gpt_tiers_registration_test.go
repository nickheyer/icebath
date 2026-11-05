package provider

import (
	"math/big"
	"reflect"
	"slices"
	"testing"
)

var gpt6Tiers = []string{"gpt-6-sol", "gpt-6-luna"}

// Context window recorded for key in a context table, false when absent
func contextTableEntry(table []contextEntry, key string) (int, bool) {
	for _, e := range table {
		if e.key == key {
			return e.ctx, true
		}
	}
	return 0, false
}

// Official docs pricing row for an openai slug, failing the test when missing
func openAIDocsPricing(t *testing.T, slug string) PricingEntry {
	t.Helper()
	entry, ok := officialDocsPricing[pricingKey{"openai", slug}]
	if !ok {
		t.Fatalf("no official pricing for %s", slug)
	}
	return entry
}

// Codex route compression threshold, nil for the Python None
func codexThreshold(model, provider string) *float64 {
	v, ok := auxCompressionThresholdForModel(model, provider, true)
	if !ok {
		return nil
	}
	return &v
}

func TestGpt56OfficialPricingReachableFromOpenAI(t *testing.T) {
	entry, ok := lookupOfficialDocsPricing(ResolveBillingRoute("gpt-5.6-sol", "openai", ""))
	if !ok || entry.InputCostPerMillion == nil {
		t.Fatalf("entry %+v %v", entry, ok)
	}
}

func TestGpt56CacheWriteIs125xInput(t *testing.T) {
	for _, slug := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		entry := openAIDocsPricing(t, slug)
		if entry.InputCostPerMillion == nil {
			t.Fatal(slug)
		}
		write := new(big.Rat).Mul(entry.InputCostPerMillion, big.NewRat(125, 100))
		read := new(big.Rat).Mul(entry.InputCostPerMillion, big.NewRat(10, 100))
		if entry.CacheWriteCostPerMillion == nil || entry.CacheWriteCostPerMillion.Cmp(write) != 0 {
			t.Fatalf("%s cache write %v", slug, entry.CacheWriteCostPerMillion)
		}
		if entry.CacheReadCostPerMillion == nil || entry.CacheReadCostPerMillion.Cmp(read) != 0 {
			t.Fatalf("%s cache read %v", slug, entry.CacheReadCostPerMillion)
		}
	}
}

func TestGpt56ProVariantsAliasToBaseTierPricing(t *testing.T) {
	for _, base := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		pro, tier := openAIDocsPricing(t, base+"-pro"), openAIDocsPricing(t, base)
		if pro.InputCostPerMillion != tier.InputCostPerMillion || !reflect.DeepEqual(pro, tier) {
			t.Fatalf("%s-pro is not the base tier row", base)
		}
	}
}

func TestGpt56AutoraiseAppliesToAllOnCodex(t *testing.T) {
	for _, slug := range []string{"gpt-5.6-sol", "gpt-5.6-sol-pro", "gpt-5.6-terra", "gpt-5.6-terra-pro", "gpt-5.6-luna", "gpt-5.6-luna-pro"} {
		if got := codexThreshold(slug, "openai-codex"); got == nil || *got != 0.85 {
			t.Fatalf("%s threshold %v", slug, got)
		}
	}
}

func TestGpt56NoAutoraiseOnDirectAPIRoute(t *testing.T) {
	if got := codexThreshold("gpt-5.6-sol", "openai"); got != nil {
		t.Fatalf("openai %v", *got)
	}
	if got := codexThreshold("openai/gpt-5.6-sol", "openrouter"); got != nil {
		t.Fatalf("openrouter %v", *got)
	}
}

func TestAstraAutoraiseOnCodexUnless900k(t *testing.T) {
	for _, slug := range []string{"gpt-6-astra", "openai/gpt-6-astra-pro", "gpt-6-astra-2026-09-01"} {
		if got := codexThreshold(slug, "openai-codex"); got == nil || *got != 0.85 {
			t.Fatalf("%s codex %v", slug, got)
		}
		if got := codexThreshold(slug, "openrouter"); got != nil {
			t.Fatalf("%s openrouter %v", slug, *got)
		}
	}
	if got := codexThreshold("gpt-6-astra-900k", "openai-codex"); got != nil {
		t.Fatalf("900k %v", *got)
	}
}

func TestGpt6TiersShareTheCodex900kContractWith56(t *testing.T) {
	ids := finalizeCodexModels([]string{"gpt-5.5"})
	sol900k, _ := verifiedCodexCtxForSlug("gpt-5.6-sol-900k")
	solThreshold := codexThreshold("gpt-5.6-sol", "openai-codex")
	for _, base := range gpt6Tiers {
		if slices.Index(ids, base+"-900k") != slices.Index(ids, base)+1 || slices.Index(ids, base) < 0 || slices.Contains(ids, base+"-pro-900k") {
			t.Fatalf("%s picker ids %v", base, ids)
		}
		if !IsCodex900kBase(base + "-2026-09-22") {
			t.Fatalf("%s dated snapshot loses eligibility", base)
		}
		if got := StripCodexContextVariantSuffix("openai/" + base + "-900k"); got != "openai/"+base {
			t.Fatalf("%s strip %q", base, got)
		}
		if got, _ := verifiedCodexCtxForSlug(base + "-900k"); got != sol900k {
			t.Fatalf("%s verified ctx %d want %d", base, got, sol900k)
		}
		if got := codexThreshold(base, "openai-codex"); got == nil || solThreshold == nil || *got != *solThreshold {
			t.Fatalf("%s threshold %v", base, got)
		}
		if got := codexThreshold(base+"-900k", "openai-codex"); got != nil {
			t.Fatalf("%s-900k threshold %v", base, *got)
		}
		if got := codexSupportedEfforts("openai/" + base); !slices.Equal(got, codexGPT56Efforts) {
			t.Fatalf("%s efforts %v", base, got)
		}
	}
}

func TestGpt61SolTakesAstraLadderWithoutAstraGating(t *testing.T) {
	for _, slug := range []string{"gpt-6.1-sol", "openai/gpt-6.1-sol-pro", "gpt-6.1-sol-2026-09-29"} {
		if got := codexSupportedEfforts(slug); !slices.Equal(got, codexAstraEfforts) || isAstraModel(slug) {
			t.Fatalf("%s efforts %v astra %v", slug, got, isAstraModel(slug))
		}
	}
	if !slices.Contains(codexSupportedEfforts("gpt-6-sol"), "none") {
		t.Fatal("gpt-6-sol lost none")
	}
}

func TestOpenRouterOmitsDisableTheOpenAILadderRejects(t *testing.T) {
	efforts := []any{"max", "xhigh", "high", "medium", "low", "none"}
	p := bundledPluginProfile(t, "openrouter")
	seedOpenRouterReasoningCaps(t, openrouterCapsItem("openai/gpt-6.1-sol", efforts, false), openrouterCapsItem("openai/gpt-6-sol", efforts, false))
	off := &Reasoning{Enabled: bptr(false)}
	body, _ := p.BuildAPIKwargsExtras(off, map[string]any{"supports_reasoning": true, "model": "openai/gpt-6.1-sol"})
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("6.1 sol sent a disable %v", body)
	}
	body, _ = p.BuildAPIKwargsExtras(off, map[string]any{"supports_reasoning": true, "model": "openai/gpt-6-sol"})
	if !reflect.DeepEqual(body["reasoning"], map[string]any{"enabled": false}) {
		t.Fatalf("6 sol reasoning %v", body)
	}
}

func TestGpt61SolResolvesContextAndPricingLikeItsTier(t *testing.T) {
	a, okA := contextTableEntry(defaultContextLengths, "gpt-6.1-sol")
	b, okB := contextTableEntry(defaultContextLengths, "gpt-6-sol")
	if !okA || !okB || a != b {
		t.Fatalf("default context %d %d", a, b)
	}
	a, okA = contextTableEntry(codexOAuthContextFallback, "gpt-6.1-sol")
	b, okB = contextTableEntry(codexOAuthContextFallback, "gpt-6-sol")
	if !okA || !okB || a != b {
		t.Fatalf("codex fallback context %d %d", a, b)
	}
	base := openAIDocsPricing(t, "gpt-6.1-sol")
	if pro := openAIDocsPricing(t, "gpt-6.1-sol-pro"); pro.InputCostPerMillion != base.InputCostPerMillion || !reflect.DeepEqual(pro, base) {
		t.Fatal("gpt-6.1-sol-pro is not the base row")
	}
	if base.CacheReadCostPerMillion.Cmp(new(big.Rat).Quo(base.InputCostPerMillion, big.NewRat(20, 1))) != 0 {
		t.Fatalf("cache read %v input %v", base.CacheReadCostPerMillion, base.InputCostPerMillion)
	}
}

func TestGpt61Sol900kIsOptInExactAndBilledAsTheBase(t *testing.T) {
	ids := finalizeCodexModels([]string{"gpt-6.1-sol"})
	if i := slices.Index(ids, "gpt-6.1-sol"); i < 0 || slices.Index(ids, "gpt-6.1-sol-900k") != i+1 {
		t.Fatalf("picker ids %v", ids)
	}
	plain := finalizeCodexModels([]string{"gpt-5.5"})
	if slices.Contains(plain, "gpt-6.1-sol") || slices.Contains(plain, "gpt-6.1-sol-900k") {
		t.Fatalf("no entitlement still lists 6.1 %v", plain)
	}
	if IsCodex900kBase("gpt-6.1-sol-pro") || !IsCodexContextVariant("openai/gpt-6.1-sol-900k") {
		t.Fatal("900k eligibility")
	}
	if got := StripCodexContextVariantSuffix("gpt-6.1-sol-900k"); got != "gpt-6.1-sol" {
		t.Fatalf("strip %q", got)
	}
	if _, ok := verifiedCodexCtxForSlug("gpt-6.1-sol"); ok {
		t.Fatal("base slug has a verified ctx")
	}
	if got, ok := verifiedCodexCtxForSlug("gpt-6.1-sol-900k"); !ok || got <= codexOAuthStaleAdvertisedCtx || got >= 922_000 {
		t.Fatalf("900k verified ctx %d %v", got, ok)
	}
	if v, b := openAIDocsPricing(t, "gpt-6.1-sol-900k"), openAIDocsPricing(t, "gpt-6.1-sol"); v.InputCostPerMillion != b.InputCostPerMillion || !reflect.DeepEqual(v, b) {
		t.Fatal("900k is not billed as the base")
	}
	if got := codexThreshold("gpt-6.1-sol-900k", "openai-codex"); got != nil {
		t.Fatalf("900k threshold %v", *got)
	}
}
