package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// Restricts listing to the Kimi alias family with offline models.dev data and the given canonical list
func stubKimiDiscovery(t *testing.T, canonical []CanonicalProviderEntry) context.Context {
	t.Helper()
	ctx, _ := pickerTestHome(t, nil)
	savedMapping, savedCanonical, savedInfo := providerToModelsDev, pickerCanonicalFn, pickerProviderInfoFn
	t.Cleanup(func() {
		providerToModelsDev, pickerCanonicalFn, pickerProviderInfoFn = savedMapping, savedCanonical, savedInfo
	})
	providerToModelsDev = map[string]string{"kimi": "kimi-for-coding", "kimi-coding": "kimi-for-coding",
		"moonshot": "kimi-for-coding", "kimi-coding-cn": "kimi-for-coding"}
	pickerFetchModelsDevFn = func(context.Context, bool) map[string]any {
		return map[string]any{"kimi-for-coding": map[string]any{"name": "Kimi For Coding", "env": []any{"KIMI_API_KEY"}}}
	}
	pickerProviderInfoFn = func(context.Context, string, bool) (ModelsDevProvider, bool) {
		return ModelsDevProvider{Name: "Kimi For Coding"}, true
	}
	pickerCanonicalFn = func() []CanonicalProviderEntry { return slices.Clone(canonical) }
	cachedProviderModelIDsFn = func(context.Context, string, CachedModelsOptions) []string { return []string{"kimi-k2.6", "kimi-k2.5"} }
	return ctx
}

func rowSlugs(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		out = append(out, pyOrStr(r["slug"]))
	}
	return out
}

func slugCount(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}

func TestSingleKimiCredentialYieldsOneCanonicalRow(t *testing.T) {
	ctx := stubKimiDiscovery(t, []CanonicalProviderEntry{{Slug: "kimi-coding", Label: "Kimi / Kimi Coding Plan", TUIDesc: "desc"}})
	t.Setenv("KIMI_API_KEY", "sk-test-kimi")
	slugs := rowSlugs(mustListProviders(t, ctx, AuthenticatedProvidersOptions{MaxModels: intPtr(10)}))
	var kimi []string
	for _, s := range slugs {
		if s == "kimi" || s == "kimi-coding" {
			kimi = append(kimi, s)
		}
	}
	if !slices.Equal(kimi, []string{"kimi-coding"}) || slugCount(slugs, "kimi-coding") != 1 || slices.Contains(slugs, "kimi") {
		t.Fatalf("expected a single canonical Kimi row, got %v", slugs)
	}
}

func TestDistinctKimiChinaCredentialStillListed(t *testing.T) {
	ctx := stubKimiDiscovery(t, []CanonicalProviderEntry{{Slug: "kimi-coding", Label: "Kimi / Kimi Coding Plan", TUIDesc: "desc"},
		{Slug: "kimi-coding-cn", Label: "Kimi / Moonshot (China)", TUIDesc: "desc"}})
	t.Setenv("KIMI_API_KEY", "sk-test-kimi")
	t.Setenv("KIMI_CN_API_KEY", "sk-test-kimi-cn")
	slugs := rowSlugs(mustListProviders(t, ctx, AuthenticatedProvidersOptions{MaxModels: intPtr(10)}))
	if slugCount(slugs, "kimi-coding") != 1 || slices.Contains(slugs, "kimi") || !slices.Contains(slugs, "kimi-coding-cn") {
		t.Fatalf("slugs %v", slugs)
	}
}

func TestNonBlockingListingOpensNoSocket(t *testing.T) {
	h := newTestHome(t)
	ResetProviderModelsCaches()
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "DEEPSEEK_API_KEY", "GROQ_API_KEY", "MISTRAL_API_KEY", "XAI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(key, "dummy")
	}
	if err := os.WriteFile(filepath.Join(h, "provider_models_cache.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var live []string
	probe := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		live = append(live, name)
	}
	savedIDs, savedFetch, savedIndex, savedSWR, savedDev := providerModelIDsFn, fetchAPIModelsFn, fetchLiveCatalogIndexFn, spawnSWRRefreshFn, pickerFetchModelsDevFn
	savedCache, savedTime := modelsDevCache, modelsDevCacheTime
	t.Cleanup(func() {
		providerModelIDsFn, fetchAPIModelsFn, fetchLiveCatalogIndexFn, spawnSWRRefreshFn, pickerFetchModelsDevFn = savedIDs, savedFetch, savedIndex, savedSWR, savedDev
		modelsDevCache, modelsDevCacheTime = savedCache, savedTime
		ResetProviderModelsCaches()
	})
	modelsDevCache, modelsDevCacheTime = map[string]any{}, time.Now()
	pickerFetchModelsDevFn = func(context.Context, bool) map[string]any { return map[string]any{} }
	providerModelIDsFn = func(_ context.Context, p string, _ bool) CatalogRows {
		probe("provider_model_ids " + p)
		return CatalogRows{}
	}
	fetchAPIModelsFn = func(_ context.Context, _, baseURL string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
		probe("fetch_api_models " + baseURL)
		return nil, false
	}
	fetchLiveCatalogIndexFn = func(_ context.Context, rawURL string, _ time.Duration) ([]any, map[string]map[string]any, bool) {
		probe("live_catalog_index " + rawURL)
		return nil, nil, false
	}
	spawnSWRRefreshFn = func(context.Context, string, func() (map[string]any, bool)) {}
	rows, err := ListPickerProviders(context.Background(), PickerProvidersOptions{MaxModels: intPtr(50), IncludeMoA: true,
		CurrentProvider: "openrouter", UserProviders: map[string]any{}, CustomProviders: []any{}, ExcludedProviders: []string{},
		NonBlockingCatalogs: true})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := slices.Clone(live)
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("cache only listing ran live probes in the request path %v", got)
	}
	row := pickerRow(rows, "openrouter")
	if row == nil || len(row["models"].([]any)) == 0 {
		t.Fatalf("OpenRouter row lost its curated snapshot %v", row)
	}
}
