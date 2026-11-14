package provider

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"
)

// Scripts the pricing table per slug, the Nous tier and the paid model partition
func invPatchPricing(freeTier bool, pricing map[string]PricingTable, unavailable []string) {
	inventoryPricingFn = func(_ context.Context, slug, _ string, _, _ bool) PricingTable { return pricing[slug] }
	inventoryCheckNousTierFn = func(context.Context, bool, bool) bool { return freeTier }
	inventoryPartitionFn = func(ids []string, _ PricingTable, _ bool) ([]string, []string) {
		var allowed []string
		for _, m := range ids {
			if !slices.Contains(unavailable, m) {
				allowed = append(allowed, m)
			}
		}
		return allowed, append([]string{}, unavailable...)
	}
	inventoryNousPickerUsageFn = func(context.Context, bool, bool) *PickerUsage { return nil }
}

func TestApplyPricingFormatsPerModelPrices(t *testing.T) {
	invTestHome(t)
	invPatchPricing(false, map[string]PricingTable{"openrouter": {
		"a/paid": {"prompt": "0.000003", "completion": "0.000015", "input_cache_read": "0.0000003"},
		"b/free": {"prompt": "0", "completion": "0"},
	}}, nil)
	rows := []map[string]any{{"slug": "openrouter", "models": []any{"a/paid", "b/free"}}}
	applyPricing(context.Background(), rows, false, false)
	pricing := rows[0]["pricing"].(map[string]any)
	if !reflect.DeepEqual(pricing["a/paid"], map[string]any{"input": "$3.00", "output": "$15.00", "cache": "$0.30", "free": false}) {
		t.Fatalf("paid %v", pricing["a/paid"])
	}
	free := pricing["b/free"].(map[string]any)
	if free["free"] != true || free["input"] != "free" {
		t.Fatalf("free %v", free)
	}
}

func TestApplyPricingFreeModelsGetFlat100PercentSale(t *testing.T) {
	invTestHome(t)
	invPatchPricing(false, map[string]PricingTable{"nous": {
		"a/free":          {"prompt": "0", "completion": "0", "original": map[string]any{"prompt": "0.000002", "completion": "0.00001"}},
		"b/natively-free": {"prompt": "0", "completion": "0"},
	}}, nil)
	rows := []map[string]any{{"slug": "nous", "models": []any{"a/free", "b/natively-free"}}}
	applyPricing(context.Background(), rows, false, false)
	pricing := rows[0]["pricing"].(map[string]any)
	free, native := pricing["a/free"].(map[string]any), pricing["b/natively-free"].(map[string]any)
	if free["free"] != true || free["discount_percent"] != 100 || free["was_input"] != "$2.00" || free["was_output"] != "$10.00" {
		t.Fatalf("free %v", free)
	}
	_, hasIn := native["was_input"]
	_, hasOut := native["was_output"]
	if native["free"] != true || native["discount_percent"] != 100 || hasIn || hasOut {
		t.Fatalf("native %v", native)
	}
}

func TestApplyPricingOmitsSaleWhenOriginalNotCheaper(t *testing.T) {
	invTestHome(t)
	invPatchPricing(false, map[string]PricingTable{"nous": {
		"a/eq": {"prompt": "0.000002", "completion": "0.00001", "original": map[string]any{"prompt": "0.000002", "completion": "0.00001"}},
	}}, nil)
	rows := []map[string]any{{"slug": "nous", "models": []any{"a/eq"}}}
	applyPricing(context.Background(), rows, false, false)
	if _, has := rows[0]["pricing"].(map[string]any)["a/eq"].(map[string]any)["discount_percent"]; has {
		t.Fatal("sale chrome without a cheaper original")
	}
}

func TestApplyPricingWindowPlanDropsPricesAndStampsPlanUsage(t *testing.T) {
	invTestHome(t)
	invPatchPricing(false, map[string]PricingTable{"nous": {
		"a/m": {"prompt": "0.000001", "completion": "0.000005", "usage_level": "medium"},
	}}, nil)
	inventoryNousPickerUsageFn = func(context.Context, bool, bool) *PickerUsage {
		return pickerUsageFromAccount(pickerWindowAccount("Window Plan", pickerKW{"day_used_bp": 1000, "week_used_bp": 500,
			"top_tier_used_bp": 0, "day_resets_at_ms": 1, "week_resets_at_ms": 2}))
	}
	rows := []map[string]any{{"slug": "nous", "models": []any{"a/m"}}}
	applyPricing(context.Background(), rows, false, false)
	plan, _ := rows[0]["plan_usage"].(map[string]any)
	_, hasPricing := rows[0]["pricing"]
	_, hasIntensity := rows[0]["cost_intensity"]
	if hasPricing || hasIntensity || plan["kind"] != "usage_window" || plan["plan"] != "Window Plan" ||
		!reflect.DeepEqual(rows[0]["usage_levels"], map[string]string{"a/m": "medium"}) {
		t.Fatalf("row %v", rows[0])
	}
}

func TestApplyPricingCreditGrantKeepsPricesAndStampsPlanUsage(t *testing.T) {
	invTestHome(t)
	invPatchPricing(false, map[string]PricingTable{"nous": {"a/m": {"prompt": "0.000001", "completion": "0.000005"}}}, nil)
	plan := "Credit Plan"
	inventoryNousPickerUsageFn = func(context.Context, bool, bool) *PickerUsage {
		return pickerUsageFromAccount(&NousPortalAccountInfo{LoggedIn: true, Subscription: &NousPortalSubscriptionInfo{Plan: &plan}})
	}
	rows := []map[string]any{{"slug": "nous", "models": []any{"a/m"}}}
	applyPricing(context.Background(), rows, false, false)
	_, hasPlan := rows[0]["plan_usage"]
	if rows[0]["pricing"].(map[string]any)["a/m"].(map[string]any)["input"] != "$1.00" || hasPlan {
		t.Fatalf("row %v", rows[0])
	}
}

func TestModelOptionsColdPricingFetchRunsOffTheRequestPath(t *testing.T) {
	invTestHome(t)
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce bool
	inventoryPricingFn = func(_ context.Context, _, _ string, _, cachedOnly bool) PricingTable {
		if cachedOnly {
			return PricingTable{}
		}
		if !startOnce {
			startOnce = true
			close(started)
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return PricingTable{}
	}
	inventoryOpenRouterCapsFn = func(context.Context, string, time.Duration, bool) (ReasoningCapabilities, bool) {
		return ReasoningCapabilities{}, false
	}
	inventoryApplyCapsFn = func(context.Context, []map[string]any, map[string]any) error { return nil }
	inventoryApplyFeaturedFn = func(context.Context, []map[string]any, map[string]any) {}
	inventoryStripOverlapsFn = func(context.Context, []map[string]any) {}
	invListReturning([]map[string]any{invRow("openrouter", "OpenRouter", []any{"vendor/model"}, true, false, "built-in")})
	inventoryPricingPrewarmMu.Lock()
	savedRuns := inventoryPricingPrewarmRuns
	inventoryPricingPrewarmRuns = map[string]chan struct{}{}
	inventoryPricingPrewarmMu.Unlock()
	var workers []chan struct{}
	defer func() {
		inventoryPricingPrewarmMu.Lock()
		for _, ch := range inventoryPricingPrewarmRuns {
			workers = append(workers, ch)
		}
		inventoryPricingPrewarmRuns = savedRuns
		inventoryPricingPrewarmMu.Unlock()
		close(release)
		for _, ch := range workers {
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
			}
		}
	}()
	startedAt := time.Now()
	payload, err := BuildModelOptionsPayload(context.Background(), invCtx("openrouter", "vendor/model", ""), false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(startedAt)
	row := payloadRows(payload)[0]
	if _, has := row["pricing"]; row["slug"] != "openrouter" || has || elapsed >= 2*time.Second {
		t.Fatalf("row %v elapsed %v", row, elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("pricing did not prewarm in the background")
	}
}

func TestColdNousEntitlementKeepsModelsUnselectable(t *testing.T) {
	invTestHome(t)
	inventoryPricingFn = func(context.Context, string, string, bool, bool) PricingTable { return PricingTable{} }
	inventoryCachedNousTierFn = func(context.Context) (bool, bool) { return false, false }
	rows := []map[string]any{{"slug": "nous", "models": []any{"free/model", "paid/model"}}}
	applyPricing(context.Background(), rows, false, true)
	if rows[0]["free_tier_pending"] != true || !reflect.DeepEqual(rows[0]["unavailable_models"], []any{"free/model", "paid/model"}) {
		t.Fatalf("row %v", rows[0])
	}
}
