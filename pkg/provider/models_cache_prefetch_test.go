package provider

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func cachedModelsOf(t *testing.T, row any) []string {
	t.Helper()
	m, _ := row.(map[string]any)
	var out []string
	switch v := m["models"].(type) {
	case []string:
		out = v
	case []any:
		for _, s := range v {
			out = append(out, s.(string))
		}
	}
	return out
}

func TestStoreCacheEntry(t *testing.T) {
	t.Run("writes new entry", func(t *testing.T) {
		ctx := modelsTestHome(t)
		storeCacheEntry(ctx, "openrouter", cacheEntry("fp1", []string{"m1", "m2"}, time.Now()))
		row := loadProviderModelsCache(ctx)["openrouter"]
		if !slices.Equal(cachedModelsOf(t, row), []string{"m1", "m2"}) || row.(map[string]any)["fp"] != "fp1" {
			t.Fatalf("row %v", row)
		}
	})
	t.Run("does not clobber other entries", func(t *testing.T) {
		ctx := modelsTestHome(t)
		storeCacheEntry(ctx, "provider_a", cacheEntry("fp_a", []string{"a1"}, time.Now()))
		storeCacheEntry(ctx, "provider_b", cacheEntry("fp_b", []string{"b1"}, time.Now()))
		cache := loadProviderModelsCache(ctx)
		if !slices.Equal(cachedModelsOf(t, cache["provider_a"]), []string{"a1"}) || !slices.Equal(cachedModelsOf(t, cache["provider_b"]), []string{"b1"}) {
			t.Fatalf("cache %v", cache)
		}
	})
	t.Run("concurrent writes no lost entries", func(t *testing.T) {
		ctx := modelsTestHome(t)
		work := make(chan string)
		var wg sync.WaitGroup
		for range 5 {
			wg.Go(func() {
				for p := range work {
					storeCacheEntry(ctx, p, cacheEntry("fp_"+p, []string{"model_" + p}, time.Now()))
				}
			})
		}
		for i := range 10 {
			work <- fmt.Sprintf("prov_%d", i)
		}
		close(work)
		wg.Wait()
		cache := loadProviderModelsCache(ctx)
		for i := range 10 {
			p := fmt.Sprintf("prov_%d", i)
			if !slices.Equal(cachedModelsOf(t, cache[p]), []string{"model_" + p}) {
				t.Fatalf("%s was lost in concurrent write %v", p, cache)
			}
		}
	})
}

// Goroutine running fn is parked on a mutex or semaphore
func goroutineBlockedIn(fn string) bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, block := range strings.Split(string(buf), "\n\n") {
		header, _, _ := strings.Cut(block, "\n")
		if strings.Contains(block, fn) && (strings.Contains(header, "sync.Mutex.Lock") || strings.Contains(header, "semacquire") || strings.Contains(header, "sleep")) {
			return true
		}
	}
	return false
}

func TestClearIsNotUndoneByAConcurrentLockedWrite(t *testing.T) {
	for _, cleared := range []string{"openrouter", ""} {
		t.Run("cleared "+cleared, func(t *testing.T) {
			ctx := modelsTestHome(t)
			saveProviderModelsCache(ctx, map[string]any{"openrouter": cacheEntry("fp", []string{"or/model"}, time.Now()),
				"anthropic": cacheEntry("fp", []string{"claude"}, time.Now())})
			loaded, release, writerDone, clearerDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(writerDone)
				writeUnderCacheLock(providerModelsCachePath(ctx), func() {
					cache := loadProviderModelsCache(ctx)
					close(loaded)
					select {
					case <-release:
					case <-time.After(5 * time.Second):
					}
					cache["anthropic"] = cacheEntry("fp", []string{"claude-new"}, time.Now())
					saveProviderModelsCache(ctx, cache)
				})
			}()
			<-loaded
			clearFn := "ClearAllProviderModelsCache"
			if cleared != "" {
				clearFn = "ClearProviderModelsCache"
			}
			go func() {
				defer close(clearerDone)
				if cleared == "" {
					ClearAllProviderModelsCache(ctx)
				} else {
					ClearProviderModelsCache(ctx, cleared)
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				select {
				case <-clearerDone:
				default:
					if !goroutineBlockedIn("provider."+clearFn) && time.Now().Before(deadline) {
						time.Sleep(5 * time.Millisecond)
						continue
					}
				}
				break
			}
			close(release)
			<-writerDone
			<-clearerDone
			if _, ok := loadProviderModelsCache(ctx)["openrouter"]; ok {
				t.Fatal("cleared row came back")
			}
		})
	}
}

type prefetchFakes struct {
	mu      sync.Mutex
	fetched []string
}

func (f *prefetchFakes) record(slug string) {
	f.mu.Lock()
	f.fetched = append(f.fetched, slug)
	f.mu.Unlock()
}

// Fixes the cache snapshot and fingerprint the prefetch gate reads, recording every forced fetch
func fakePrefetchGate(t *testing.T, cache map[string]any, fp string, fetch func(slug string) []string) *prefetchFakes {
	t.Helper()
	modelsTestHome(t)
	savedLoad, savedFP, savedCached := loadProviderModelsCacheFn, credentialFingerprintFn, cachedProviderModelIDsFn
	t.Cleanup(func() {
		loadProviderModelsCacheFn, credentialFingerprintFn, cachedProviderModelIDsFn = savedLoad, savedFP, savedCached
	})
	if cache != nil {
		loadProviderModelsCacheFn = func(context.Context) map[string]any { return cache }
	}
	credentialFingerprintFn = func(context.Context, string) string { return fp }
	fakes := &prefetchFakes{}
	cachedProviderModelIDsFn = func(_ context.Context, slug string, opts CachedModelsOptions) []string {
		if !opts.ForceRefresh {
			t.Errorf("prefetch of %s without force refresh", slug)
		}
		fakes.record(slug)
		if fetch == nil {
			return []string{"model_" + slug}
		}
		return fetch(slug)
	}
	return fakes
}

func nowAt(offset time.Duration) float64 { return float64(time.Now().Add(offset).UnixNano()) / 1e9 }

func TestPrefetchProviderModelsParallel(t *testing.T) {
	t.Run("skips all fresh entries", func(t *testing.T) {
		fakes := fakePrefetchGate(t, map[string]any{"openrouter": cacheEntry("fp", []string{"m1"}, time.Now()),
			"anthropic": cacheEntry("fp", []string{"m2"}, time.Now())}, "fp", nil)
		prefetchProviderModelsParallel(context.Background(), []string{"openrouter", "anthropic"})
		if len(fakes.fetched) != 0 {
			t.Fatalf("fetched %v", fakes.fetched)
		}
	})
	t.Run("fetches only stale entries", func(t *testing.T) {
		fakes := fakePrefetchGate(t, map[string]any{"fresh_prov": cacheEntry("fp_f", []string{"m1"}, time.Now())}, "fp_f", nil)
		prefetchProviderModelsParallel(context.Background(), []string{"fresh_prov", "stale_prov"})
		if slices.Contains(fakes.fetched, "fresh_prov") || !slices.Contains(fakes.fetched, "stale_prov") {
			t.Fatalf("fetched %v", fakes.fetched)
		}
	})
	t.Run("fetches in parallel", func(t *testing.T) {
		var mu sync.Mutex
		active, maxConcurrent := 0, 0
		fakePrefetchGate(t, map[string]any{}, "fp", func(slug string) []string {
			mu.Lock()
			active++
			maxConcurrent = max(maxConcurrent, active)
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return []string{"model_" + slug}
		})
		var slugs []string
		for i := range 6 {
			slugs = append(slugs, fmt.Sprintf("prov_%d", i))
		}
		prefetchProviderModelsParallel(context.Background(), slugs)
		if maxConcurrent <= 1 {
			t.Fatal("fetches were serial, not parallel")
		}
	})
	t.Run("a failed fetch is absorbed", func(t *testing.T) {
		fakes := fakePrefetchGate(t, map[string]any{}, "fp", func(string) []string { return nil })
		prefetchProviderModelsParallel(context.Background(), []string{"failing_prov"})
		if !slices.Equal(fakes.fetched, []string{"failing_prov"}) {
			t.Fatalf("fetched %v", fakes.fetched)
		}
	})
	t.Run("empty list is noop", func(t *testing.T) {
		fakes := fakePrefetchGate(t, nil, "fp", nil)
		prefetchProviderModelsParallel(context.Background(), nil)
		if len(fakes.fetched) != 0 {
			t.Fatalf("fetched %v", fakes.fetched)
		}
	})
	t.Run("skips ttl expired entries the serial path can still serve", func(t *testing.T) {
		row := cacheEntry("fp", []string{"m1"}, time.Now())
		row["at"] = nowAt(-providerModelsCacheTTL - time.Minute)
		fakes := fakePrefetchGate(t, map[string]any{"openrouter": row}, "fp", nil)
		prefetchProviderModelsParallel(context.Background(), []string{"openrouter"})
		if len(fakes.fetched) != 0 {
			t.Fatalf("fetched %v", fakes.fetched)
		}
	})
	t.Run("fetches curated fallback rows past their short ttl", func(t *testing.T) {
		row := cacheEntry("fp", []string{"m1"}, time.Now())
		row["at"], row["fallback"] = nowAt(-2*time.Hour), true
		fakes := fakePrefetchGate(t, map[string]any{"openrouter": row}, "fp", func(string) []string { return []string{"m1"} })
		prefetchProviderModelsParallel(context.Background(), []string{"openrouter"})
		if !slices.Equal(fakes.fetched, []string{"openrouter"}) {
			t.Fatalf("fetched %v", fakes.fetched)
		}
	})
}

// Real cached reads over a fake live catalog with a fixed fingerprint
func fakePrefetchLive(t *testing.T, live func(ctx context.Context, slug string) CatalogRows) context.Context {
	t.Helper()
	ctx := modelsTestHome(t)
	savedFP, savedIDs := credentialFingerprintFn, providerModelIDsFn
	t.Cleanup(func() { credentialFingerprintFn, providerModelIDsFn = savedFP, savedIDs })
	credentialFingerprintFn = func(context.Context, string) string { return "fp" }
	providerModelIDsFn = func(ctx context.Context, slug string, _ bool) CatalogRows { return live(ctx, slug) }
	return ctx
}

func TestSlowWorkerDoesNotDropRowsPersistedDuringItsFetch(t *testing.T) {
	ctx := fakePrefetchLive(t, func(ctx context.Context, slug string) CatalogRows {
		if slug != "openrouter" {
			deadline := time.Now().Add(10 * time.Second)
			for loadProviderModelsCache(ctx)["openrouter"] == nil {
				if time.Now().After(deadline) {
					t.Error("openrouter row never persisted")
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		return plainRows([]string{slug + "/model"})
	})
	prefetchProviderModelsParallel(ctx, []string{"openrouter", "anthropic"})
	var keys []string
	for k := range loadProviderModelsCache(ctx) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"anthropic", "openrouter"}) {
		t.Fatalf("keys %v", keys)
	}
}

func TestFailedLiveFetchLeavesTheStaleRowUntouched(t *testing.T) {
	ctx := fakePrefetchLive(t, func(context.Context, string) CatalogRows { return CatalogRows{} })
	staleAt := nowAt(-providerModelsStaleServeMax - time.Hour)
	saveProviderModelsCache(ctx, map[string]any{"openrouter": map[string]any{"fp": "fp", "at": staleAt, "models": []any{"openrouter/model"}}})
	prefetchProviderModelsParallel(ctx, []string{"openrouter"})
	if row, _ := loadProviderModelsCache(ctx)["openrouter"].(map[string]any); row == nil || row["at"] != staleAt {
		t.Fatalf("row %v want at %v", row, staleAt)
	}
}
