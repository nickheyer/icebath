package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

const plsReset int64 = 4_000_000_000_000

func plsPricing() map[string]any {
	return map[string]any{
		"example/model-a":         map[string]any{"prompt": "0.00000001", "completion": "0.0000001", "name": "Model A"},
		"example/model-b":         map[string]any{"prompt": "0.0000004", "completion": "0.000004", "name": "Model B"},
		"example/model-c":         map[string]any{"prompt": "0.000005", "completion": "0.000025", "top_tier": true, "name": "Model C"},
		"example/model-free:free": map[string]any{"prompt": "0", "completion": "0", "name": "Model Free"},
	}
}

var plsFeatured = []string{"example/model-a", "example/model-b", "example/model-c", "example/model-free:free"}

func plsSet(day, week, top int) []WindowGauge {
	return []WindowGauge{plGauge("day", day, ScopeAll, plsReset), plGauge("week", week, ScopeAll, plsReset), plGauge("top_tier", top, ScopeTopTier, plsReset)}
}

var (
	plsDayFull = plsSet(10_000, 4_000, 2_000)
	plsTopFull = plsSet(3_000, 2_000, 10_000)
	plsHealthy = plsSet(3_000, 2_000, 2_000)
)

// Scripts the plan caches like the Python autouse fixture, every seam restored after the test
func plsNoCaches(t *testing.T) {
	prevPricing, prevFeatured, prevFree, prevLen, prevFits, prevSuggest := planCachedPricingFn, planFeaturedModelsFn, planPortalFreeFn,
		planContextLengthFn, planFitsContextFn, planSuggestModelsFn
	planPortalFreeFn = func(context.Context) []string { return nil }
	planFeaturedModelsFn = func() []string { return plsFeatured }
	planCachedPricingFn = func(string) map[string]any { return plsPricing() }
	planFitsContextFn = func(context.Context, string, int, string) bool { return true }
	t.Cleanup(func() {
		planCachedPricingFn, planFeaturedModelsFn, planPortalFreeFn, planContextLengthFn, planFitsContextFn, planSuggestModelsFn = prevPricing,
			prevFeatured, prevFree, prevLen, prevFits, prevSuggest
	})
}

func plsSuggest(current string, gauges []WindowGauge, featured []string, needed int) []ModelSuggestion {
	return SuggestModels(context.Background(), current, gauges, plsPricing(), featured, needed, "", planMaxSuggestions)
}

func plsModels(out []ModelSuggestion) string {
	var ids []string
	for _, s := range out {
		ids = append(ids, s.Model)
	}
	return strings.Join(ids, ",")
}

func TestPlanLimitSuggestTopTierLimitOffersIncludedRecommendedModelsInPlanOrder(t *testing.T) {
	plsNoCaches(t)
	out := plsSuggest("example/model-c", plsTopFull, plsFeatured, 0)
	if len(out) != 2 || out[0] != (ModelSuggestion{"example/model-a", "Model A", NoteIncluded, "nous"}) ||
		out[1] != (ModelSuggestion{"example/model-b", "Model B", NoteIncluded, "nous"}) {
		t.Fatalf("%+v", out)
	}
}

func TestPlanLimitSuggestWindowLimitOffersOnlyFreeModels(t *testing.T) {
	plsNoCaches(t)
	out := plsSuggest("example/model-b", plsDayFull, plsFeatured, 0)
	if len(out) != 1 || out[0].Model != "example/model-free:free" || out[0].Note != NoteFree {
		t.Fatalf("%+v", out)
	}
}

func TestPlanLimitSuggestPortalFreePicksFollowThePlanList(t *testing.T) {
	plsNoCaches(t)
	planPortalFreeFn = func(context.Context) []string { return []string{"example/model-free:free", "other/y:free"} }
	if got := plsModels(plsSuggest("example/model-b", plsDayFull, plsFeatured, 0)); got != "example/model-free:free,other/y:free" {
		t.Fatalf("%s", got)
	}
}

func TestPlanLimitSuggestNeverTheCurrentModel(t *testing.T) {
	plsNoCaches(t)
	if out := plsSuggest("example/model-free:free", plsDayFull, []string{"example/model-free:free"}, 0); len(out) != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestPlanLimitSuggestAtMostTwo(t *testing.T) {
	plsNoCaches(t)
	if out := plsSuggest("example/model-b", plsDayFull, []string{"a/1:free", "a/2:free", "a/3:free"}, 0); len(out) != 2 {
		t.Fatalf("%+v", out)
	}
}

func TestPlanLimitSuggestModelWhoseContextCannotHoldTheConversationIsSkipped(t *testing.T) {
	plsNoCaches(t)
	planFitsContextFn = func(_ context.Context, model string, _ int, _ string) bool { return model != "example/model-a" }
	if got := plsModels(plsSuggest("example/model-c", plsTopFull, plsFeatured, 500_000)); got != "example/model-b,example/model-free:free" {
		t.Fatalf("%s", got)
	}
}

func TestPlanLimitSuggestFitsContextLeavesHeadroomForTheReply(t *testing.T) {
	plsNoCaches(t)
	planContextLengthFn = func(context.Context, ContextLengthRequest) int { return 100_000 }
	ctx := context.Background()
	if !fitsPlanContext(ctx, "m", 90_000, "") || fitsPlanContext(ctx, "m", 95_000, "") || !fitsPlanContext(ctx, "m", 0, "") {
		t.Fatal("headroom")
	}
}

func plsAgent(model string, gauges []WindowGauge) *CreditsCapture {
	c := NewCreditsCapture(nil, "nous", model, "https://inference-api.nousresearch.com/v1", "")
	s := NewCreditsState()
	s.DenominatorKind, s.Gauges = "usage_window", gauges
	c.credits = s
	return c
}

func TestPlanLimitHitTopTierRefusal(t *testing.T) {
	plsNoCaches(t)
	hit := LimitHitFor(context.Background(), plsAgent("example/model-c", plsTopFull), "You've hit your monthly large-model limit", 0)
	if hit == nil || hit.GaugeID != "top_tier" || hit.Scope != ScopeTopTier || hit.LimitName != "monthly large-model" || *hit.ResetsAtMs != plsReset ||
		plsModels(hit.Suggestions) != "example/model-a,example/model-b" {
		t.Fatalf("%+v", hit)
	}
	p := hit.ToPayload()
	first := fmt.Sprint(p["suggestions"])
	if p["message"] != "You've hit your monthly large-model limit" ||
		!strings.HasPrefix(first, "[map[label:Model A model:example/model-a note:included provider:nous]") ||
		fmt.Sprint(p["credits"]) != "map[action:add_credits]" || p["current_model"] != "example/model-c" {
		t.Fatalf("%v", p)
	}
}

func TestPlanLimitHitDailyRefusal(t *testing.T) {
	plsNoCaches(t)
	hit := LimitHitFor(context.Background(), plsAgent("example/model-b", plsDayFull), "x", 0)
	if hit == nil || hit.GaugeID != "day" || hit.LimitName != "daily" || plsModels(hit.Suggestions) != "example/model-free:free" {
		t.Fatalf("%+v", hit)
	}
}

func TestPlanLimitHitUnmarkedModelRefusedWithTheTopTierSpentStillExplainsIt(t *testing.T) {
	plsNoCaches(t)
	planCachedPricingFn = func(string) map[string]any { return map[string]any{} }
	if hit := LimitHitFor(context.Background(), plsAgent("example/model-c", plsTopFull), "x", 0); hit == nil || hit.GaugeID != "top_tier" {
		t.Fatalf("%+v", hit)
	}
}

func TestPlanLimitHitNoFullGaugeOrNoWindowPlanIsNoHit(t *testing.T) {
	plsNoCaches(t)
	if hit := LimitHitFor(context.Background(), plsAgent("example/model-b", plsHealthy), "x", 0); hit != nil {
		t.Fatalf("%+v", hit)
	}
	notWindow := NewCreditsCapture(nil, "nous", "m", "", "")
	notWindow.credits = NewCreditsState()
	if hit := LimitHitFor(context.Background(), notWindow, "x", 0); hit != nil {
		t.Fatalf("%+v", hit)
	}
	if hit := LimitHitFor(context.Background(), NewCreditsCapture(nil, "nous", "", "", ""), "x", 0); hit != nil {
		t.Fatalf("%+v", hit)
	}
}

func TestPlanLimitHitNeverRaises(t *testing.T) {
	plsNoCaches(t)
	planSuggestModelsFn = func(context.Context, string, []WindowGauge, map[string]any, []string, int, string, int) []ModelSuggestion {
		panic("cache broke")
	}
	if hit := LimitHitFor(context.Background(), plsAgent("example/model-b", plsDayFull), "x", 0); hit != nil {
		t.Fatalf("%+v", hit)
	}
}

func TestPlanLimitFreePicksComeFromThisProfilesOwnPortalEntry(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	ctx := home.WithOverride(context.Background(), tmp)
	portal := resolveNousPortalURL(ctx)
	nousRecommendedMu.Lock()
	saved := nousRecommendedCache
	nousRecommendedCache = map[nousRecommendedKey]nousRecommendedEntry{}
	other := map[string]any{"freeRecommendedModels": []any{map[string]any{"modelName": "other/profile:free"}}}
	mine := map[string]any{"freeRecommendedModels": []any{map[string]any{"modelName": "mine/model:free"}}}
	nousRecommendedCache[nousRecommendedKey{"other-profile", portal}] = nousRecommendedEntry{data: other}
	nousRecommendedMu.Unlock()
	t.Cleanup(func() { nousRecommendedMu.Lock(); nousRecommendedCache = saved; nousRecommendedMu.Unlock() })
	if got := portalFreeModels(ctx); len(got) != 0 {
		t.Fatalf("another profile answered: %v", got)
	}
	nousRecommendedMu.Lock()
	nousRecommendedCache[nousRecommendedKey{pricingProfileKey(ctx), portal}] = nousRecommendedEntry{data: mine}
	nousRecommendedCache[nousRecommendedKey{"other-profile", "https://other-portal.test"}] = nousRecommendedEntry{data: other}
	nousRecommendedMu.Unlock()
	if got := strings.Join(portalFreeModels(ctx), ","); got != "mine/model:free" {
		t.Fatalf("%s", got)
	}
}
