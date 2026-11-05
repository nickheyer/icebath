package provider

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Offline picker home with real overlays, curated catalogs, a no op pricing prewarm and an empty usage cache
func optionsHome(t *testing.T) (context.Context, string) {
	t.Helper()
	overlays := icebathOverlays
	ctx, _ := pickerTestHome(t, nil)
	icebathOverlays = overlays
	providerModelIDsFn = func(_ context.Context, p string, _ bool) CatalogRows { return CatalogRows{IDs: staticCatalogModels(p)} }
	savedPrewarm := inventoryPrewarmFn
	inventoryPrewarmFn = func(context.Context, []map[string]any, string, string) <-chan struct{} { return nil }
	t.Cleanup(func() { inventoryPrewarmFn = savedPrewarm })
	accountUsageCache.Lock()
	accountUsageCache.snapshots = map[accountUsageKey]*AccountUsageSnapshot{}
	accountUsageCache.started = map[accountUsageKey]time.Time{}
	accountUsageCache.lastTry = map[accountUsageKey]time.Time{}
	accountUsageCache.inflight = map[accountUsageKey]bool{}
	accountUsageCache.Unlock()
	return ctx, home.Dir(ctx)
}

func optionsRows(t *testing.T, ctx context.Context, pc PickerConfigContext, explicitOnly, refresh bool) []map[string]any {
	t.Helper()
	payload, err := BuildModelOptionsPayload(ctx, pc, explicitOnly, false, refresh)
	if err != nil {
		t.Fatal(err)
	}
	return payloadRows(payload)
}

func TestPickerReadsCachedUsageAndRefreshesWithoutWaiting(t *testing.T) {
	ctx, _ := optionsHome(t)
	t.Setenv("OPENROUTER_API_KEY", "test-key-123")
	release, called := make(chan struct{}), make(chan struct{})
	var answered sync.WaitGroup
	answered.Add(1)
	var once sync.Once
	saved := accountUsageFetchers["openrouter"]
	accountUsageFetchers["openrouter"] = func(context.Context, string, string) (*AccountUsageSnapshot, error) {
		once.Do(func() { close(called) })
		<-release
		defer answered.Done()
		used, reset := 92.0, time.Now().UTC().Add(2*time.Hour)
		return &AccountUsageSnapshot{Provider: "openrouter", Source: "test", FetchedAt: time.Now().UTC(),
			Windows: []AccountUsageWindow{{Label: "Current session", UsedPercent: &used, ResetAt: &reset}}}, nil
	}
	pc := invLoadContext(t, ctx)
	first := pickerRow(optionsRows(t, ctx, pc, false, false), "openrouter")
	if first == nil {
		t.Fatal("no openrouter row")
	}
	if _, ok := first["usage"]; ok {
		t.Fatal("nothing cached yet, so nothing to show")
	}
	select {
	case <-called:
	case <-time.After(30 * time.Second):
		t.Fatal("opening the picker must start a background usage refresh")
	}
	close(release)
	answered.Wait()
	deadline := time.Now().Add(30 * time.Second)
	for CachedAccountUsage(ctx, "openrouter", "", "") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the background refresh never cached the usage snapshot")
		}
		time.Sleep(20 * time.Millisecond)
	}
	accountUsageFetchers["openrouter"] = saved
	row := pickerRow(optionsRows(t, ctx, pc, false, false), "openrouter")
	usage, _ := row["usage"].(map[string]any)
	windows, _ := usage["windows"].([]any)
	if len(windows) != 1 {
		t.Fatalf("usage %v", row["usage"])
	}
	window := windows[0].(map[string]any)
	if window["label"] != "Current session" || window["used_percent"] != 92.0 {
		t.Fatalf("window %v", window)
	}
	resets, err := time.Parse(time.RFC3339Nano, fmt.Sprint(window["resets_at"]))
	if err != nil || !resets.After(time.Now()) {
		t.Fatalf("resets_at %v %v", window["resets_at"], err)
	}
}

// One authed models.dev provider whose live probe is recorded and optionally hung
func readPathHome(t *testing.T, hung string) (context.Context, func() []string, chan struct{}) {
	t.Helper()
	ctx, _ := pickerTestHome(t, nil)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	cachedProviderModelIDsFn = CachedProviderModelIDs
	release := make(chan struct{})
	var mu sync.Mutex
	var calls []string
	providerModelIDsFn = func(_ context.Context, p string, _ bool) CatalogRows {
		mu.Lock()
		calls = append(calls, p)
		mu.Unlock()
		if p == hung {
			<-release
		}
		return CatalogRows{IDs: []string{"live-model"}}
	}
	pickerFetchModelsDevFn = func(context.Context, bool) map[string]any {
		return map[string]any{"deepseek": map[string]any{"env": []any{"DEEPSEEK_API_KEY"}, "name": "DeepSeek"}}
	}
	savedMap, savedPrewarm := providerToModelsDev, inventoryPrewarmFn
	providerToModelsDev = map[string]string{"deepseek": "deepseek"}
	inventoryPrewarmFn = func(context.Context, []map[string]any, string, string) <-chan struct{} { return nil }
	t.Cleanup(func() {
		WaitProviderModelsRefreshes()
		providerToModelsDev, inventoryPrewarmFn = savedMap, savedPrewarm
	})
	return ctx, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}, release
}

func TestDegradedProviderCannotStallTheOpen(t *testing.T) {
	ctx, calls, release := readPathHome(t, "deepseek")
	pc := invLoadContext(t, ctx)
	done := make(chan []map[string]any, 1)
	go func() {
		payload, err := BuildModelOptionsPayload(ctx, pc, false, false, false)
		if err != nil {
			t.Error(err)
		}
		done <- payloadRows(payload)
	}()
	var rows []map[string]any
	returnedWhileHung := false
	// The probe holds until release, so any return before it proves the open never waited on the probe
	select {
	case rows = <-done:
		returnedWhileHung = true
	case <-time.After(3 * time.Minute):
	}
	close(release)
	if !returnedWhileHung {
		rows = <-done
	}
	WaitProviderModelsRefreshes()
	if !returnedWhileHung {
		t.Fatal("the open waited on a degraded provider's catalog probe")
	}
	row := pickerRow(rows, "deepseek")
	if row == nil || len(rowModels(row)) == 0 {
		t.Fatalf("the degraded row must still render from curated %v", row)
	}
	if containsString(rowModels(row), "live-model") {
		t.Fatalf("models %v", row["models"])
	}
	if !containsString(calls(), "deepseek") {
		t.Fatal("the degraded provider is still probed off the read path")
	}
}

func TestExplicitRefreshStillProbesProviders(t *testing.T) {
	ctx, calls, _ := readPathHome(t, "")
	rows := optionsRows(t, ctx, invLoadContext(t, ctx), false, true)
	if !containsString(calls(), "deepseek") {
		t.Fatal("an explicit refresh must still probe provider catalogs")
	}
	if row := pickerRow(rows, "deepseek"); row == nil || !containsString(rowModels(row), "live-model") {
		t.Fatalf("row %v", row)
	}
}

// Writes a one entry credential pool for the provider in the cooldown state named
func writeCooldownPool(t *testing.T, h, provider, cooldown, model string) map[string]any {
	t.Helper()
	now := float64(time.Now().UnixNano()) / 1e9
	oauth := provider == "anthropic"
	entry := map[string]any{"id": "e1", "label": "subscription", "priority": 0, "last_status": "ok",
		"auth_type":     map[bool]string{true: "oauth", false: "api_key"}[oauth],
		"source":        map[bool]string{true: "manual:icebath_pkce", false: "manual"}[oauth],
		"access_token":  map[bool]string{true: "sk-ant-oat01-test", false: "test-key-123"}[oauth],
		"refresh_token": nil, "expires_at_ms": int64((now + 6*3600) * 1000)}
	if oauth {
		entry["refresh_token"] = "refresh"
	}
	switch cooldown {
	case "exhausted":
		entry["last_status"], entry["last_status_at"], entry["last_error_code"] = "exhausted", now, 429
		entry["last_error_reset_at"] = now + 3*3600
	case "model_cooldown":
		entry["model_cooldowns"] = map[string]any{model: now + 3*3600}
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{provider: []any{entry}}})
	return entry
}

// Home whose only credential is the pooled one under test, with a google models.dev row for gemini
func cooldownHome(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx, h := optionsHome(t)
	writeConfigYAML(t, h, "model:\n  provider: nous\n  default: test-model\nauth:\n  adopt_external_logins: false\n")
	pickerFetchModelsDevFn = func(context.Context, bool) map[string]any {
		return map[string]any{"google": map[string]any{"env": []any{"GOOGLE_API_KEY", "GEMINI_API_KEY"}, "name": "Google"}}
	}
	return ctx, h
}

func TestRateLimitedPoolKeepsItsProviderRow(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini"} {
		for _, cooldown := range []string{"exhausted", "model_cooldown"} {
			for _, explicitOnly := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s-%s-%v", provider, cooldown, explicitOnly), func(t *testing.T) {
					ctx, h := cooldownHome(t)
					writeCooldownPool(t, h, provider, cooldown, "some-model")
					row := pickerRow(optionsRows(t, ctx, invLoadContext(t, ctx), explicitOnly, false), provider)
					if row == nil {
						t.Fatalf("a %s %s pool dropped the provider from model.options", cooldown, provider)
					}
					if len(rowModels(row)) == 0 {
						t.Fatal("the kept row must still offer the provider's models")
					}
				})
			}
		}
	}
}

func isoNear(t *testing.T, iso any, want float64) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, fmt.Sprint(iso))
	if err != nil {
		t.Fatalf("iso %v %v", iso, err)
	}
	if got := float64(ts.UnixNano()) / 1e9; math.Abs(got-want) > 1 {
		t.Fatalf("reset %v want %v", got, want)
	}
}

func TestKeptRowSaysWhichLimitAndWhenItResets(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			ctx, h := cooldownHome(t)
			pc := invLoadContext(t, ctx)
			writeCooldownPool(t, h, provider, "ok", "some-model")
			healthy := pickerRow(optionsRows(t, ctx, pc, false, false), provider)
			if healthy == nil {
				t.Fatal("no healthy row")
			}
			if _, ok := healthy["limit"]; ok {
				t.Fatalf("healthy limit %v", healthy["limit"])
			}
			models := rowModels(healthy)
			model, sibling := models[0], models[1]
			entry := writeCooldownPool(t, h, provider, "exhausted", "some-model")
			limit := pickerRow(optionsRows(t, ctx, pc, false, false), provider)["limit"].(map[string]any)
			if limit["scope"] != "account" {
				t.Fatalf("limit %v", limit)
			}
			isoNear(t, limit["resets_at"], entry["last_error_reset_at"].(float64))
			entry = writeCooldownPool(t, h, provider, "model_cooldown", model)
			limit = pickerRow(optionsRows(t, ctx, pc, false, false), provider)["limit"].(map[string]any)
			tagged, _ := limit["models"].(map[string]any)
			if limit["scope"] != "models" || len(tagged) != 1 || tagged[model] == nil {
				t.Fatalf("%s still works and must not be tagged %v", sibling, limit)
			}
			isoNear(t, tagged[model], entry["model_cooldowns"].(map[string]any)[model].(float64))
		})
	}
}

func TestPickerVisibilityKeepsTheFullCustomProbeBudget(t *testing.T) {
	ctx, _ := optionsHome(t)
	providerModelIDsFn = func(context.Context, string, bool) CatalogRows { return CatalogRows{} }
	saved := pickerFetchLiveModelsFn
	t.Cleanup(func() { pickerFetchLiveModelsFn = saved })
	pickerFetchLiveModelsFn = func(_ context.Context, _ any, _, _ string, _ bool, _ map[string]string, timeout time.Duration, _ string, _ bool) (CatalogRows, bool) {
		if timeout >= 5*time.Second {
			return CatalogRows{IDs: []string{"slow-discovered-model"}}, true
		}
		return CatalogRows{}, false
	}
	pc := invLoadContext(t, ctx).WithOverrides("custom", "kept-model", "http://127.0.0.1:9999/v1")
	custom := pickerRow(optionsRows(t, ctx, pc, false, false), "custom")
	if custom == nil || !containsString(rowModels(custom), "slow-discovered-model") {
		t.Fatalf("custom %v", custom)
	}
}
