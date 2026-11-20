package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func validManifest() map[string]any {
	raw := `{"version": 1, "updated_at": "2026-04-25T22:00:00Z", "metadata": {"source": "test"},
	 "providers": {
	   "openrouter": {"metadata": {"display_name": "OpenRouter"}, "models": [
	     {"id": "anthropic/claude-opus-4.7", "description": "recommended"},
	     {"id": "openai/gpt-5.4", "description": ""},
	     {"id": "openrouter/elephant-alpha", "description": "free"}]},
	   "ai-gateway": {"metadata": {"display_name": "Vercel AI Gateway"}, "models": [
	     {"id": "anthropic/claude-opus-4.7"}, {"id": "moonshotai/kimi-k2.6"}]}}}`
	data, err := decodeJSONBody([]byte(raw))
	if err != nil {
		panic(err)
	}
	return data.(map[string]any)
}

type catalogFixture struct {
	ctx             context.Context
	home            string
	calls           []string
	openRouterFetch int
}

// Isolated home, reset caches, a fixed config and a recording fetch seam
func newCatalogFixture(t *testing.T, cfg CatalogConfig) *catalogFixture {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, tmp)
	f := &catalogFixture{ctx: home.WithOverride(context.Background(), tmp), home: tmp}
	ResetCatalogCache()
	prevLoad, prevFetch, prevClock := loadCatalogConfig, fetchCatalogManifest, catalogClock
	if cfg.URL == "" {
		cfg = ParseCatalogConfig(nil)
	}
	loadCatalogConfig = func(context.Context) CatalogConfig { return cfg }
	prevOR := fetchOpenRouterModelsFn
	fetchOpenRouterModelsFn = func(context.Context, bool) []CatalogModel { f.openRouterFetch++; return nil }
	t.Cleanup(func() {
		WaitCatalogRefreshes()
		loadCatalogConfig, fetchCatalogManifest, catalogClock = prevLoad, prevFetch, prevClock
		fetchOpenRouterModelsFn = prevOR
		ResetCatalogCache()
	})
	return f
}

func (f *catalogFixture) serve(fn func(url string) map[string]any) {
	fetchCatalogManifest = func(url string, timeout time.Duration) map[string]any {
		f.calls = append(f.calls, url)
		return fn(url)
	}
}

func (f *catalogFixture) writeDisk(t *testing.T, manifest map[string]any, mtime time.Time) {
	t.Helper()
	path := CatalogCachePath(f.ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func count(calls []string, url string) int {
	n := 0
	for _, c := range calls {
		if c == url {
			n++
		}
	}
	return n
}

func TestValidateCatalogManifest(t *testing.T) {
	if !ValidateCatalogManifest(validManifest()) {
		t.Fatal("well formed manifest rejected")
	}
	for _, bad := range []any{"string", []any{}, nil} {
		if ValidateCatalogManifest(bad) {
			t.Fatalf("accepted %v", bad)
		}
	}
	m := validManifest()
	models := m["providers"].(map[string]any)["openrouter"].(map[string]any)["models"].([]any)
	models[0] = map[string]any{"id": json.Number("42")}
	if ValidateCatalogManifest(m) {
		t.Fatal("non string model id accepted")
	}
	future := validManifest()
	future["version"] = json.Number("2")
	if ValidateCatalogManifest(future) {
		t.Fatal("future schema accepted")
	}
	fractional := validManifest()
	fractional["version"] = json.Number("1.0")
	if ValidateCatalogManifest(fractional) {
		t.Fatal("float version accepted")
	}
}

func TestFetchAndCacheWritesDisk(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	f.serve(func(string) map[string]any { return validManifest() })
	result := GetCatalog(f.ctx, true)
	if !reflect.DeepEqual(result, validManifest()) || len(f.calls) == 0 {
		t.Fatalf("result %v calls %v", result, f.calls)
	}
	raw, err := os.ReadFile(CatalogCachePath(f.ctx))
	if err != nil {
		t.Fatal(err)
	}
	onDisk, _ := decodeJSONBody(raw)
	if !reflect.DeepEqual(onDisk, validManifest()) {
		t.Fatal("disk cache differs from the fetched manifest")
	}
}

func TestNetworkFailureReturnsShippedCatalogWhenNoCache(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	f.serve(func(string) map[string]any { return nil })
	if got := GetCatalog(f.ctx, true); !reflect.DeepEqual(got, shippedCatalog()) {
		t.Fatalf("got %v", got)
	}
}

func TestNetworkFailureFallsBackToDiskCache(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	f.serve(func(string) map[string]any { return validManifest() })
	GetCatalog(f.ctx, true)
	ResetCatalogCache()
	f.serve(func(string) map[string]any { return nil })
	if got := GetCatalog(f.ctx, true); !reflect.DeepEqual(got, validManifest()) {
		t.Fatalf("got %v", got)
	}
}

func TestFetchFailureFallsBackToStaleCache(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	f.writeDisk(t, validManifest(), time.Now().Add(-30*24*time.Hour))
	f.serve(func(string) map[string]any { return nil })
	if got := GetCatalog(f.ctx, false); !reflect.DeepEqual(got, validManifest()) {
		t.Fatalf("got %v", got)
	}
}

func TestOfflineColdCacheDoesNotRefetchOnEveryCall(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	now := time.Now()
	catalogClock = func() time.Time { return now }
	f.serve(func(string) map[string]any { return nil })
	if !reflect.DeepEqual(GetCatalog(f.ctx, false), shippedCatalog()) {
		t.Fatal("expected the shipped catalog")
	}
	perAttempt := len(f.calls)
	if perAttempt < 1 {
		t.Fatal("no fetch attempted")
	}
	GetCatalog(f.ctx, false)
	GetCatalog(f.ctx, false)
	if len(f.calls) != perAttempt {
		t.Fatal("retried the unreachable catalog inside the window")
	}
	GetCatalog(f.ctx, true)
	if len(f.calls) != 2*perAttempt {
		t.Fatalf("explicit refresh must retry: %d", len(f.calls))
	}
	later := now.Add(CatalogFetchRetryDelay + time.Minute)
	catalogClock = func() time.Time { return later }
	f.serve(func(string) map[string]any { return validManifest() })
	if got := GetCatalog(f.ctx, false); !reflect.DeepEqual(got, validManifest()) {
		t.Fatalf("after the window: %v", got)
	}
}

func TestModelRefreshRetriesABackedOffCatalog(t *testing.T) {
	override := validManifest()
	override["providers"].(map[string]any)["openrouter"].(map[string]any)["models"] = []any{map[string]any{"id": "vendor/override-model", "description": ""}}
	f := newCatalogFixture(t, CatalogConfig{Enabled: true, URL: "http://master", TTLHours: 24, Providers: map[string]any{"openrouter": map[string]any{"url": "http://override"}}})
	f.serve(func(string) map[string]any { return nil })
	var shipped []CatalogModel
	for _, row := range catalogBlockIDs(catalogBlockOf(shippedCatalog(), "openrouter")) {
		shipped = append(shipped, CatalogModel{ID: row.id, Description: stringOf(row.entry["description"])})
	}
	if got := CuratedOpenRouterModels(f.ctx); len(shipped) == 0 || !reflect.DeepEqual(got, shipped) {
		t.Fatalf("expected the shipped rows, got %v", got)
	}
	f.serve(func(url string) map[string]any {
		if url == "http://override" {
			return override
		}
		return validManifest()
	})
	ClearCatalogFetchBackoff(f.ctx)
	if got := CuratedOpenRouterModels(f.ctx); !reflect.DeepEqual(got, []CatalogModel{{ID: "vendor/override-model"}}) {
		t.Fatalf("got %v", got)
	}
}

func TestOverrideFailureBacksOffPerProfile(t *testing.T) {
	cfg := CatalogConfig{Enabled: true, URL: "http://master", TTLHours: 24, Providers: map[string]any{"openrouter": map[string]any{"url": "http://override"}}}
	f := newCatalogFixture(t, cfg)
	alpha := home.WithOverride(context.Background(), filepath.Join(f.home, "alpha"))
	beta := home.WithOverride(context.Background(), filepath.Join(f.home, "beta"))
	f.serve(func(string) map[string]any { return nil })
	CuratedOpenRouterModels(alpha)
	f.serve(func(string) map[string]any { return validManifest() })
	CuratedOpenRouterModels(beta)
	if count(f.calls, "http://override") < 2 {
		t.Fatalf("profile beta skipped the override after alpha's failure: %v", f.calls)
	}
}

func TestDeadOverrideOnTheManifestURLKeepsTheFallbackChain(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{Enabled: true, URL: "http://master", TTLHours: 24, Providers: map[string]any{"openrouter": map[string]any{"url": "http://master"}}})
	f.serve(func(url string) map[string]any {
		if url == "http://master" {
			return nil
		}
		return validManifest()
	})
	got := CuratedOpenRouterModels(f.ctx)
	want := []CatalogModel{{"anthropic/claude-opus-4.7", "recommended"}, {"openai/gpt-5.4", ""}, {"openrouter/elephant-alpha", "free"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestUnreachableProviderOverrideIsNotRefetchedOnEveryCall(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{Enabled: true, URL: "http://master", TTLHours: 24, Providers: map[string]any{"openrouter": map[string]any{"url": "http://override"}}})
	f.serve(func(url string) map[string]any {
		if url == "http://override" {
			return nil
		}
		return validManifest()
	})
	first := CuratedOpenRouterModels(f.ctx)
	second := CuratedOpenRouterModels(f.ctx)
	want := []CatalogModel{{"anthropic/claude-opus-4.7", "recommended"}, {"openai/gpt-5.4", ""}, {"openrouter/elephant-alpha", "free"}}
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Fatalf("%v %v", first, second)
	}
	if count(f.calls, "http://override") != 1 {
		t.Fatalf("override asked %d times", count(f.calls, "http://override"))
	}
}

const (
	catalogPrimary  = "https://icebath.app/api/model-catalog.json"
	catalogFallback = "https://raw.githubusercontent.com/nickheyer/icebath/main/pkg/provider/model-catalog.json"
)

func TestFallbackChainUsesPrimaryWhenItSucceeds(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	f.serve(func(string) map[string]any { return validManifest() })
	if fetchCatalogManifestWithFallback(catalogPrimary, 5*time.Second, DefaultCatalogFallbackURLs) == nil {
		t.Fatal("nil result")
	}
	if !reflect.DeepEqual(f.calls, []string{catalogPrimary}) {
		t.Fatalf("fallback URLs touched: %v", f.calls)
	}
}

func TestFallbackChainFallsThroughToRawGithubOnPrimaryFailure(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	f.serve(func(url string) map[string]any {
		if url == catalogPrimary {
			return nil
		}
		return validManifest()
	})
	if fetchCatalogManifestWithFallback(catalogPrimary, 5*time.Second, DefaultCatalogFallbackURLs) == nil {
		t.Fatal("nil result")
	}
	if !reflect.DeepEqual(f.calls, []string{catalogPrimary, catalogFallback}) {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestGetCatalogUsesFallbackChain(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	f.serve(func(url string) map[string]any {
		if url == catalogPrimary {
			return nil
		}
		return validManifest()
	})
	if got := GetCatalog(f.ctx, true); !reflect.DeepEqual(got, validManifest()) || count(f.calls, catalogFallback) != 1 {
		t.Fatalf("got %v calls %v", got, f.calls)
	}
}

func TestCuratedAccessors(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	f.serve(func(string) map[string]any { return validManifest() })
	want := []CatalogModel{{"anthropic/claude-opus-4.7", "recommended"}, {"openai/gpt-5.4", ""}, {"openrouter/elephant-alpha", "free"}}
	if got := CuratedOpenRouterModels(f.ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func manifestWithDefault() map[string]any {
	m := validManifest()
	providers := m["providers"].(map[string]any)
	providers["openrouter"].(map[string]any)["models"].([]any)[1].(map[string]any)["default"] = true
	providers["ai-gateway"].(map[string]any)["models"].([]any)[1].(map[string]any)["default"] = true
	return m
}

func TestDefaultModelFromCatalogCache(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	f.writeDisk(t, manifestWithDefault(), time.Time{})
	f.serve(func(string) map[string]any { t.Fatal("fetched"); return nil })
	if got := DefaultModelFromCatalogCache(f.ctx, "openrouter"); got != "openai/gpt-5.4" {
		t.Fatalf("openrouter default %q", got)
	}
	if got := DefaultModelFromCatalogCache(f.ctx, "ai-gateway"); got != "moonshotai/kimi-k2.6" {
		t.Fatalf("ai-gateway default %q", got)
	}
	f.writeDisk(t, validManifest(), time.Time{})
	if got := DefaultModelFromCatalogCache(f.ctx, "openrouter"); got != "" {
		t.Fatalf("unlabeled default %q", got)
	}
}

func TestProviderOverrideURLTakesPrecedence(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{Enabled: true, URL: "http://master", TTLHours: 24, Providers: map[string]any{"openrouter": map[string]any{"url": "http://override"}}})
	override := map[string]any{"version": json.Number("1"), "providers": map[string]any{"openrouter": map[string]any{"models": []any{map[string]any{"id": "override/model", "description": "custom"}}}}}
	f.serve(func(url string) map[string]any {
		if url == "http://override" {
			return override
		}
		return validManifest()
	})
	if got := CuratedOpenRouterModels(f.ctx); !reflect.DeepEqual(got, []CatalogModel{{"override/model", "custom"}}) {
		t.Fatalf("got %v", got)
	}
}

func TestDefaultTTLIsTwentyMinutesAndLegacyHoursHonoured(t *testing.T) {
	if got := ParseCatalogConfig(map[string]any{"ttl_minutes": 20}); got.TTLHours*3600 != 20*60 {
		t.Fatalf("ttl %v", got.TTLHours)
	}
	if got := ParseCatalogConfig(map[string]any{"ttl_minutes": 20, "ttl_hours": 3}); got.TTLHours != 3 {
		t.Fatalf("legacy ttl %v", got.TTLHours)
	}
	if got := ParseCatalogConfig(map[string]any{"ttl_minutes": 5, "ttl_hours": 3}); got.TTLHours*60 != 5 {
		t.Fatalf("user minutes must win: %v", got.TTLHours)
	}
	if got := ParseCatalogConfig(map[string]any{"ttl_minutes": -1, "url": " ", "enabled": false}); got.TTLHours*60 != 20 || got.URL != DefaultCatalogURL || got.Enabled {
		t.Fatalf("defaults %+v", got)
	}
	f := newCatalogFixture(t, CatalogConfig{Enabled: true, URL: "http://master", TTLHours: 20.0 / 60.0, Providers: map[string]any{}})
	if CatalogRefreshInterval(f.ctx) != 20*time.Minute {
		t.Fatal("interval")
	}
	loadCatalogConfig = func(context.Context) CatalogConfig { return CatalogConfig{Enabled: true, URL: "x", TTLHours: 0.001} }
	if CatalogRefreshInterval(f.ctx) != time.Minute {
		t.Fatal("floor")
	}
}

func TestRefreshCatalogsForcesEverySource(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{Enabled: true, URL: "http://master", TTLHours: 1, Providers: map[string]any{}})
	f.serve(func(string) map[string]any { return validManifest() })
	if !RefreshCatalogs(f.ctx, nil) {
		t.Fatal("expected true")
	}
	if count(f.calls, "http://master") != 1 || f.openRouterFetch != 1 {
		t.Fatalf("calls %v openrouter %d", f.calls, f.openRouterFetch)
	}
	loadCatalogConfig = func(context.Context) CatalogConfig { return CatalogConfig{Enabled: false} }
	if RefreshCatalogs(f.ctx, nil) {
		t.Fatal("disabled catalog refreshed")
	}
}

func TestStaleDiskCatalogServedWithBackgroundRefresh(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{Enabled: true, URL: "https://example/cat.json", TTLHours: 1, Providers: map[string]any{}})
	stale := map[string]any{"version": json.Number("1"), "providers": map[string]any{"ai-gateway": map[string]any{"models": []any{map[string]any{"id": "openai/gpt-5.4"}}}}}
	f.writeDisk(t, stale, time.Now().Add(-2*time.Hour))
	fresh := validManifest()
	fetched := make(chan struct{}, 1)
	f.serve(func(string) map[string]any { fetched <- struct{}{}; return fresh })
	out := GetCatalog(f.ctx, false)
	if !reflect.DeepEqual(out, stale) {
		t.Fatalf("stale copy not served: %v", out)
	}
	WaitCatalogRefreshes()
	select {
	case <-fetched:
	default:
		t.Fatal("background refresh never ran")
	}
	if got, _ := readCatalogDiskCache(f.ctx); !reflect.DeepEqual(got, fresh) {
		t.Fatal("refresh did not land on disk")
	}
	if got := GetCatalog(f.ctx, false); !reflect.DeepEqual(got, fresh) {
		t.Fatal("next read did not pick up the refreshed manifest")
	}
}

func TestColdCacheStillBlocksOnFetch(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{Enabled: true, URL: "https://example/cat.json", TTLHours: 1, Providers: map[string]any{}})
	manifest := map[string]any{"version": json.Number("1"), "providers": map[string]any{}}
	f.serve(func(string) map[string]any { return manifest })
	if got := GetCatalog(f.ctx, false); !reflect.DeepEqual(got, manifest) || len(f.calls) != 1 {
		t.Fatalf("got %v calls %v", got, f.calls)
	}
	catalogSWRMu.Lock()
	inflight := len(catalogSWRInflight)
	catalogSWRMu.Unlock()
	if inflight != 0 {
		t.Fatal("cold cache must not spawn a refresh")
	}
}

func TestSeedCatalogCacheFromCheckout(t *testing.T) {
	f := newCatalogFixture(t, CatalogConfig{})
	root := t.TempDir()
	dir := filepath.Join(root, "pkg", "provider")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if SeedCatalogCacheFromCheckout(f.ctx, root) {
		t.Fatal("missing manifest seeded")
	}
	os.WriteFile(filepath.Join(dir, "model-catalog.json"), []byte("{}"), 0o600)
	if SeedCatalogCacheFromCheckout(f.ctx, root) {
		t.Fatal("invalid manifest seeded")
	}
	raw, _ := json.Marshal(validManifest())
	os.WriteFile(filepath.Join(dir, "model-catalog.json"), raw, 0o600)
	if !SeedCatalogCacheFromCheckout(f.ctx, root) {
		t.Fatal("seed failed")
	}
	if got, _ := readCatalogDiskCache(f.ctx); !reflect.DeepEqual(got, validManifest()) {
		t.Fatal("seed not on disk")
	}
}

// WaitCatalogRefreshes blocks until every background manifest refresh has landed
func WaitCatalogRefreshes() { catalogSWRWait.Wait() }
