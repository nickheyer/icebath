package provider

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

func onSaleList() map[string]any {
	return map[string]any{"prompt": "0.000002", "completion": "0.00001"}
}

func onSale(pctOff int) map[string]any {
	now := 0.000002 * float64(100-pctOff) / 100
	return map[string]any{"prompt": fmt.Sprintf("%.10f", now), "completion": "0.00001", "original": onSaleList()}
}

func withTools(row map[string]any, tools bool) map[string]any {
	row["tools"] = tools
	return row
}

func TestUnionWithNousOnSaleCases(t *testing.T) {
	ids := UnionWithNousOnSaleModels([]string{"curated/a"}, PricingTable{"curated/a": onSale(40), "sale/small": onSale(20),
		"sale/deep": onSale(80), "full/price": onSaleList()})
	if !slices.Equal(ids, []string{"curated/a", "sale/deep", "sale/small"}) {
		t.Fatalf("deepest first %v", ids)
	}
	skipped := PricingTable{"sale/no-tools": withTools(onSale(70), false),
		"free/with-original": {"prompt": "0", "completion": "0", "original": onSaleList()},
		"free/native":        {"prompt": "0", "completion": "0"}}
	if got := UnionWithNousOnSaleModels([]string{"c"}, skipped); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("tool less and free rows %v", got)
	}
	if got := UnionWithNousOnSaleModels([]string{"a", "b"}, PricingTable{}); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("no pricing %v", got)
	}
	almost := PricingTable{"sale/almost-free": {"prompt": "0.000000001", "completion": "0.000000001", "original": onSaleList()},
		"free/zero": {"prompt": "0", "completion": "0", "original": onSaleList()}}
	if got := UnionWithNousOnSaleModels(nil, almost); !slices.Equal(got, []string{"sale/almost-free"}) {
		t.Fatalf("rounding to full discount %v", got)
	}
}

// Serves one models payload to every pricing catalog read
func serveOnSaleCatalog(t *testing.T, data []any) {
	t.Helper()
	newPricingFixture(t)
	pricingGetJSON = func(string, map[string]string, time.Duration) map[string]any { return map[string]any{"data": data} }
}

func fetchOnSalePricing(apiKey string, includeSaleOriginal bool) PricingTable {
	return FetchModelsWithPricing(context.Background(), PricingFetchOptions{APIKey: apiKey, BaseURL: "https://example.test", ForceRefresh: true,
		IncludeSaleOriginal: includeSaleOriginal})
}

func TestNousFetchFlagsToolLessRowsSoTheUnionCanSkipThem(t *testing.T) {
	serveOnSaleCatalog(t, []any{
		map[string]any{"id": "t/yes", "supported_parameters": []any{"tools"}, "pricing": onSale(50)},
		map[string]any{"id": "t/no", "supported_parameters": []any{"temperature"}, "pricing": onSale(50)},
		map[string]any{"id": "t/unknown", "pricing": onSale(50)},
	})
	if got := UnionWithNousOnSaleModels(nil, fetchOnSalePricing("sk-test", true)); !slices.Equal(got, []string{"t/unknown", "t/yes"}) {
		t.Fatalf("union %v", got)
	}
	for id, row := range fetchOnSalePricing("sk-test", false) {
		if _, ok := row["tools"]; ok {
			t.Fatalf("%s carries the Nous only tools flag", id)
		}
	}
}

func TestSkipsGenerationModelsOnSale(t *testing.T) {
	serveOnSaleCatalog(t, []any{
		map[string]any{"id": "chat/ok", "pricing": onSale(50)},
		map[string]any{"id": "gen/typed", "capabilities": map[string]any{"type": "image"}, "pricing": onSale(90)},
		map[string]any{"id": "vendor/flux-pro", "pricing": onSale(90)},
	})
	if got := UnionWithNousOnSaleModels(nil, fetchOnSalePricing("sk-test", true)); !slices.Equal(got, []string{"chat/ok"}) {
		t.Fatalf("union %v", got)
	}
}

func TestZeroPromptWithMissingOrInvalidCompletionIsNotAPaidSale(t *testing.T) {
	rows := []any{map[string]any{"id": "free/missing", "pricing": map[string]any{"prompt": "0", "original": onSaleList()}}}
	for i, value := range []any{"", nil, "not-a-price", "NaN", "Infinity", "-1", "0"} {
		rows = append(rows, map[string]any{"id": fmt.Sprintf("free/invalid-%d", i),
			"pricing": map[string]any{"prompt": "0", "completion": value, "original": onSaleList()}})
	}
	serveOnSaleCatalog(t, rows)
	if got := UnionWithNousOnSaleModels(nil, fetchOnSalePricing("synthetic", true)); len(got) != 0 {
		t.Fatalf("union %v", got)
	}
}

// Fixes the Nous portal pricing, tier, recommendations and org policy the GUI picker reads
func stubNousPortal(t *testing.T, freeTier bool, pricing PricingTable, allowed map[string]bool) context.Context {
	t.Helper()
	newTestHome(t)
	savedPricing, savedTier, savedRec, savedPolicy := pickerNousPricingFn, checkNousFreeTierFn, fetchNousRecommendedModelsFn, nousPolicyAllowedIDsFn
	t.Cleanup(func() {
		pickerNousPricingFn, checkNousFreeTierFn, fetchNousRecommendedModelsFn, nousPolicyAllowedIDsFn = savedPricing, savedTier, savedRec, savedPolicy
	})
	pickerNousPricingFn = func(context.Context) PricingTable { return pricing }
	checkNousFreeTierFn = func(context.Context, bool, bool) bool { return freeTier }
	fetchNousRecommendedModelsFn = func(context.Context, string, bool) map[string]any { return nil }
	nousPolicyAllowedIDsFn = func(context.Context, bool) (map[string]bool, bool) { return allowed, allowed != nil }
	return context.Background()
}

func TestGUIPickerOnSaleModels(t *testing.T) {
	pricing := PricingTable{"nous/a": onSaleList(), "sale/x": onSale(80)}
	curated := map[string][]string{"nous": {"nous/a"}}
	t.Run("paid tier shows on sale model", func(t *testing.T) {
		ctx := stubNousPortal(t, false, pricing, nil)
		if got := nousPickerModelIDs(ctx, curated, false); !slices.Equal(got, []string{"nous/a", "sale/x"}) {
			t.Fatalf("ids %v", got)
		}
	})
	t.Run("free tier does not list paid sales", func(t *testing.T) {
		ctx := stubNousPortal(t, true, pricing, nil)
		if got := nousPickerModelIDs(ctx, curated, false); slices.Contains(got, "sale/x") {
			t.Fatalf("ids %v", got)
		}
	})
	t.Run("org policy still narrows on sale models", func(t *testing.T) {
		ctx := stubNousPortal(t, false, pricing, map[string]bool{"nous/a": true})
		if got := nousPickerModelIDs(ctx, curated, false); slices.Contains(got, "sale/x") {
			t.Fatalf("ids %v", got)
		}
	})
}

func TestLoginAndGUIPaidPickersAgree(t *testing.T) {
	pricing := PricingTable{"curated/only": onSaleList(), "sale/x": onSale(80), "sale/y": onSale(30)}
	login := loginPickerIDs(t, false, pricing, nil)
	ctx := stubNousPortal(t, false, pricing, nil)
	gui := nousPickerModelIDs(ctx, map[string][]string{"nous": {"curated/only"}}, false)
	want := []string{"curated/only", "sale/x", "sale/y"}
	if !slices.Equal(login, want) || !slices.Equal(gui, want) {
		t.Fatalf("login %v gui %v", login, gui)
	}
}
