package provider

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Scripts the pricing table per slug
func invPatchPricing(pricing map[string]PricingTable) {
	inventoryPricingFn = func(_ context.Context, slug, _ string, _, _ bool) PricingTable { return pricing[slug] }
}

func TestApplyPricingFormatsPerModelPrices(t *testing.T) {
	invTestHome(t)
	invPatchPricing(map[string]PricingTable{"openrouter": {
		"a/paid": {"prompt": "0.000003", "completion": "0.000015", "input_cache_read": "0.0000003"},
		"b/free": {"prompt": "0", "completion": "0"},
	}})
	rows := []map[string]any{{"slug": "openrouter", "models": []any{"a/paid", "b/free"}}}
	applyPricing(context.Background(), rows, false)
	pricing := rows[0]["pricing"].(map[string]any)
	if !reflect.DeepEqual(pricing["a/paid"], map[string]any{"input": "$3.00", "output": "$15.00", "cache": "$0.30", "free": false}) {
		t.Fatalf("paid %v", pricing["a/paid"])
	}
	free := pricing["b/free"].(map[string]any)
	if free["free"] != true || free["input"] != "free" {
		t.Fatalf("free %v", free)
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
