package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type pickerFetchCall struct {
	apiKey, baseURL, apiMode string
	timeout                  time.Duration
	headers                  map[string]string
}

// Temp home with offline catalogs, no models.dev data, no overlays and a recorder for endpoint probes
func pickerTestHome(t *testing.T, live func(apiKey, baseURL string) ([]string, bool)) (context.Context, *[]pickerFetchCall) {
	t.Helper()
	newTestHome(t)
	savedDev, savedOverlays, savedFetch, savedCached := pickerFetchModelsDevFn, icebathOverlays, fetchAPIModelsFn, cachedProviderModelIDsFn
	savedIDs, savedNous, savedCloud, savedPrefetch, savedPool := providerModelIDsFn, curatedNousModelIDsFn, pickerOllamaCloudFn, pickerPrefetchFn, pickerLoadPoolFn
	t.Cleanup(func() {
		pickerFetchModelsDevFn, icebathOverlays, fetchAPIModelsFn, cachedProviderModelIDsFn = savedDev, savedOverlays, savedFetch, savedCached
		providerModelIDsFn, curatedNousModelIDsFn, pickerOllamaCloudFn, pickerPrefetchFn, pickerLoadPoolFn = savedIDs, savedNous, savedCloud, savedPrefetch, savedPool
		ResetProviderModelsCaches()
	})
	ResetProviderModelsCaches()
	calls := &[]pickerFetchCall{}
	pickerFetchModelsDevFn = func(context.Context, bool) map[string]any { return map[string]any{} }
	icebathOverlays = map[string]IcebathOverlay{}
	fetchAPIModelsFn = func(_ context.Context, apiKey, baseURL string, timeout time.Duration, apiMode string, headers map[string]string) ([]string, bool) {
		*calls = append(*calls, pickerFetchCall{apiKey: apiKey, baseURL: baseURL, apiMode: apiMode, timeout: timeout, headers: headers})
		if live == nil {
			return nil, false
		}
		return live(apiKey, baseURL)
	}
	cachedProviderModelIDsFn = func(context.Context, string, CachedModelsOptions) []string { return []string{} }
	providerModelIDsFn = func(context.Context, string, bool) CatalogRows { return CatalogRows{} }
	curatedNousModelIDsFn = func(context.Context) []string { return []string{} }
	pickerOllamaCloudFn = func(context.Context, OllamaCloudFetch) []string { return nil }
	pickerPrefetchFn = func(context.Context, []string) {}
	return context.Background(), calls
}

func intPtr(n int) *int { return &n }

func mustListProviders(t *testing.T, ctx context.Context, opts AuthenticatedProvidersOptions) []map[string]any {
	t.Helper()
	rows, err := ListAuthenticatedProviders(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func pickerRow(rows []map[string]any, slug string) map[string]any {
	for _, r := range rows {
		if r["slug"] == slug {
			return r
		}
	}
	return nil
}

func rowModels(row map[string]any) []string {
	var out []string
	for _, m := range row["models"].([]any) {
		out = append(out, m.(string))
	}
	return out
}

func userConfigRows(rows []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r["source"] == "user-config" {
			out = append(out, r)
		}
	}
	return out
}

func TestUserProviderRowListsEveryDeclaredModel(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "local-ollama", MaxModels: intPtr(50),
		UserProviders: map[string]any{"local-ollama": map[string]any{"name": "Local Ollama", "api": "http://localhost:11434/v1",
			"default_model": "minimax-m2.7:cloud", "models": []any{"minimax-m2.7:cloud", "kimi-k2.5:cloud", "glm-5.1:cloud", "qwen3.5:cloud"}}}})
	row := pickerRow(rows, "local-ollama")
	if row == nil || row["is_user_defined"] != true || row["total_models"] != 4 {
		t.Fatalf("row %v", row)
	}
	if got := rowModels(row); !reflect.DeepEqual(got, []string{"minimax-m2.7:cloud", "kimi-k2.5:cloud", "glm-5.1:cloud", "qwen3.5:cloud"}) {
		t.Fatalf("models %v", got)
	}
}

func TestUserProviderRowEnumeratesDictModels(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "local-ollama", MaxModels: intPtr(50),
		UserProviders: map[string]any{"local-ollama": map[string]any{"name": "Local Ollama", "api": "http://localhost:11434/v1",
			"default_model": "minimax-m2.7:cloud", "models": map[string]any{"minimax-m2.7:cloud": map[string]any{"context_length": 196608},
				"kimi-k2.5:cloud": map[string]any{"context_length": 200000}, "glm-5.1:cloud": map[string]any{"context_length": 202752}}}}})
	row := pickerRow(rows, "local-ollama")
	if row == nil || row["total_models"] != 3 {
		t.Fatalf("row %v", row)
	}
	if got := rowModels(row); !reflect.DeepEqual(got, []string{"minimax-m2.7:cloud", "glm-5.1:cloud", "kimi-k2.5:cloud"}) {
		t.Fatalf("models %v", got)
	}
}

func TestUserProviderRowPrefersLiveModels(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) {
		return []string{"old-configured-model", "new-live-model"}, true
	})
	t.Setenv("CRS_TEST_KEY", "sk-test")
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "crs-henkee", MaxModels: intPtr(50),
		UserProviders: map[string]any{"crs-henkee": map[string]any{"name": "CRS Henkee", "base_url": "http://127.0.0.1:3000/api/v1",
			"key_env": "CRS_TEST_KEY", "model": "old-configured-model",
			"models": map[string]any{"old-configured-model": map[string]any{"context_length": 200000}}}}})
	want := []pickerFetchCall{{apiKey: "sk-test", baseURL: "http://127.0.0.1:3000/api/v1", timeout: 5 * time.Second}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls %+v", *calls)
	}
	row := pickerRow(rows, "crs-henkee")
	if got := rowModels(row); !reflect.DeepEqual(got, []string{"old-configured-model", "new-live-model"}) || row["total_models"] != 2 {
		t.Fatalf("row %v", row)
	}
}

func TestUserProviderRowAcceptsBaseURLAndSingularModel(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom", MaxModels: intPtr(50),
		UserProviders: map[string]any{"custom": map[string]any{"base_url": "http://example.com/v1", "model": "gpt-5.4",
			"models": map[string]any{"gpt-5.4": map[string]any{}, "grok-4.20-beta": map[string]any{}, "minimax-m2.7": map[string]any{}}}}})
	row := pickerRow(rows, "custom")
	if row == nil || row["api_url"] != "http://example.com/v1" || row["total_models"] != 3 {
		t.Fatalf("row %v", row)
	}
	if got := rowModels(row); !reflect.DeepEqual(got, []string{"gpt-5.4", "grok-4.20-beta", "minimax-m2.7"}) {
		t.Fatalf("models %v", got)
	}
}

func TestUserProviderRowSplitsCommaDefaultModelChain(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	chain := "deepseek-v4-flash, deepseek-v4-pro, glm-5.2, deepseek-v4-flash"
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "volcengine-agent-plan", MaxModels: intPtr(50),
		UserProviders: map[string]any{"volcengine-agent-plan": map[string]any{"name": "VOLCENGINE-AGENT-PLAN",
			"base_url": "https://ark.cn-beijing.volces.com/api/v3", "default_model": chain}}})
	row := pickerRow(rows, "volcengine-agent-plan")
	if got := rowModels(row); !reflect.DeepEqual(got, []string{chain, "deepseek-v4-flash", "deepseek-v4-pro", "glm-5.2"}) || row["total_models"] != 4 {
		t.Fatalf("row %v", row)
	}
}

func TestUserAndCustomProviderOverlapYieldsOneRow(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom", MaxModels: intPtr(50),
		UserProviders: map[string]any{"custom": map[string]any{"base_url": "http://example.com/v1", "model": "gpt-5.4",
			"models": map[string]any{"gpt-5.4": map[string]any{}, "grok-4.20-beta": map[string]any{}}}},
		CustomProviders: []any{map[string]any{"name": "custom", "base_url": "http://example.com/v1", "model": "legacy-only-model"}}})
	var matches []map[string]any
	for _, r := range rows {
		if r["slug"] == "custom" {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 || !reflect.DeepEqual(rowModels(matches[0]), []string{"gpt-5.4", "grok-4.20-beta"}) {
		t.Fatalf("matches %v", matches)
	}
}

func TestNoDuplicateLabelsAcrossProviderSchemas(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	user := map[string]any{}
	var custom []any
	for _, e := range [][2]string{{"endpoint-a", "http://a.local/v1"}, {"endpoint-b", "http://b.local/v1"}, {"endpoint-c", "http://c.local/v1"}} {
		user[e[0]] = map[string]any{"name": e[0], "base_url": e[1], "model": "m1"}
		custom = append(custom, map[string]any{"name": e[0], "base_url": e[1], "model": "m1"})
	}
	rows := userConfigRows(mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "none", MaxModels: intPtr(50),
		UserProviders: user, CustomProviders: custom}))
	labels := map[string]bool{}
	for _, r := range rows {
		labels[strings.ToLower(r["name"].(string))] = true
	}
	if len(rows) != 3 || len(labels) != 3 {
		t.Fatalf("rows %v", rows)
	}
}

func TestCustomEntryOnEnvOverriddenBuiltinEndpointIsHidden(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	t.Setenv("DASHSCOPE_API_KEY", "sk-test")
	t.Setenv("DASHSCOPE_BASE_URL", "https://custom-dashscope.example.com/v1")
	pickerFetchModelsDevFn = func(context.Context, bool) map[string]any {
		return map[string]any{"alibaba": map[string]any{"name": "Alibaba Cloud (DashScope)", "env": []any{"DASHSCOPE_API_KEY"}}}
	}
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "alibaba", MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "my-dashscope-override", "base_url": "https://custom-dashscope.example.com/v1",
			"api_key": "sk-test", "model": "qwen3.6-plus"}}})
	if pickerRow(rows, "alibaba") == nil {
		t.Fatalf("no alibaba row %v", rows)
	}
	for _, r := range rows {
		if strings.Contains(r["slug"].(string), "my-dashscope-override") {
			t.Fatalf("shadow row kept %v", r)
		}
	}
}

func TestSectionThreeProbesKeylessEndpointWithoutModels(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) {
		return []string{"live-model-1", "live-model-2", "live-model-3"}, true
	})
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "local-llamacpp", MaxModels: intPtr(50),
		UserProviders: map[string]any{"local-llamacpp": map[string]any{"name": "Local llama.cpp", "api": "http://localhost:8080/v1"}}})
	if len(*calls) != 1 || (*calls)[0].apiKey != "" || (*calls)[0].timeout != 5*time.Second || (*calls)[0].apiMode != "" || (*calls)[0].headers != nil {
		t.Fatalf("calls %+v", *calls)
	}
	row := pickerRow(rows, "local-llamacpp")
	if !reflect.DeepEqual(rowModels(row), []string{"live-model-1", "live-model-2", "live-model-3"}) || row["total_models"] != 3 {
		t.Fatalf("row %v", row)
	}
}

func TestCurrentCustomModelLeadsOnlyTheCurrentRow(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	t.Setenv("OPENROUTER_API_KEY", "sk-test")
	t.Setenv("NOUS_API_KEY", "sk-test")
	cachedProviderModelIDsFn = func(_ context.Context, slug string, _ CachedModelsOptions) []string {
		if slug == "openrouter" {
			return []string{"anthropic/claude-opus-4.8", "openai/gpt-5.5"}
		}
		return []string{"curated/one"}
	}
	custom := "some-vendor/totally-custom-model-v9"
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", CurrentModel: custom})
	row := pickerRow(rows, "openrouter")
	if row == nil || rowModels(row)[0] != custom || row["total_models"] != 3 {
		t.Fatalf("openrouter row %v", row)
	}
	for _, r := range rows {
		if r["slug"] != "openrouter" && r["is_current"] != true && anyListContains(r["models"].([]any), custom) {
			t.Fatalf("leaked into %v", r["slug"])
		}
	}
}

func TestSameEndpointSameCredentialEntriesFoldToOneRow(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{}, true })
	rows := userConfigRows(mustListProviders(t, ctx, AuthenticatedProvidersOptions{MaxModels: intPtr(50), UserProviders: map[string]any{
		"palantir-claude46": map[string]any{"name": "Palantir Claude 4.6 Opus", "base_url": "https://foundry.example.com/anthropic",
			"key_env": "PALANTIR_TOKEN", "api_mode": "anthropic_messages", "model": "ri.language-model-service..language-model.anthropic-claude-4-6-opus"},
		"palantir-claude47": map[string]any{"name": "Palantir Claude 4.7 Opus", "base_url": "https://foundry.example.com/anthropic",
			"key_env": "PALANTIR_TOKEN", "api_mode": "anthropic_messages", "model": "ri.language-model-service..language-model.anthropic-claude-4-7-opus"}}}))
	if len(rows) != 1 || rows[0]["slug"] != "palantir-claude46" || rows[0]["name"] != "Palantir Claude" || len(rowModels(rows[0])) != 2 {
		t.Fatalf("rows %v", rows)
	}
}

func TestDifferentExtraHeadersKeepDistinctRows(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{}, true })
	rows := userConfigRows(mustListProviders(t, ctx, AuthenticatedProvidersOptions{MaxModels: intPtr(50), UserProviders: map[string]any{
		"tenant-a": map[string]any{"name": "Tenant A", "base_url": "https://proxy.example.com/v1", "key_env": "PROXY_TOKEN",
			"api_mode": "openai_chat", "extra_headers": map[string]any{"X-Tenant": "a"}, "model": "model-a"},
		"tenant-b": map[string]any{"name": "Tenant B", "base_url": "https://proxy.example.com/v1", "key_env": "PROXY_TOKEN",
			"api_mode": "openai_chat", "extra_headers": map[string]any{"X-Tenant": "b"}, "model": "model-b"}}}))
	if len(rows) != 2 {
		t.Fatalf("rows %v", rows)
	}
}

func TestPalantirRIDStrippedToTrailingSlug(t *testing.T) {
	if got := FormatModelForDisplay("ri.language-model-service..language-model.anthropic-claude-4-7-opus"); got != "anthropic-claude-4-7-opus" {
		t.Fatalf("got %q", got)
	}
}

type fakePickerPool struct{ available bool }

func (f fakePickerPool) HasCredentials() bool       { return true }
func (f fakePickerPool) HasAvailable(_ string) bool { return f.available }

func writeOpencodePoolStore(t *testing.T) {
	t.Helper()
	store := `{"version": 1, "providers": {}, "active_provider": null, "credential_pool": {"opencode-go": {"entries": [{"id": "x"}]}}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "auth.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExhaustedPoolProviderIsNotAuthenticated(t *testing.T) {
	overlays := icebathOverlays
	ctx, _ := pickerTestHome(t, nil)
	icebathOverlays = overlays
	writeOpencodePoolStore(t)
	pickerLoadPoolFn = func(_ context.Context, provider string) pickerPool { return fakePickerPool{available: true} }
	if slugs := GetAuthenticatedProviderSlugs(ctx, "alibaba", nil, nil); !containsString(slugs, "opencode-go") {
		t.Fatalf("an available pool lists the provider %v", slugs)
	}
	pickerLoadPoolFn = func(_ context.Context, provider string) pickerPool {
		return fakePickerPool{available: provider != "opencode-go"}
	}
	if slugs := GetAuthenticatedProviderSlugs(ctx, "alibaba", nil, nil); containsString(slugs, "opencode-go") {
		t.Fatalf("slugs %v", slugs)
	}
}

func TestPickerShowsExhaustedPoolProvider(t *testing.T) {
	overlays := icebathOverlays
	ctx, _ := pickerTestHome(t, nil)
	icebathOverlays = overlays
	writeOpencodePoolStore(t)
	pickerLoadPoolFn = func(_ context.Context, provider string) pickerPool {
		return fakePickerPool{available: provider != "opencode-go"}
	}
	rows, err := ListPickerProviders(ctx, PickerProvidersOptions{CurrentProvider: "alibaba", UserProviders: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if pickerRow(rows, "opencode-go") == nil {
		t.Fatalf("picker must show the exhausted pool provider %v", rows)
	}
}

func TestOpaqueLegacyPoolValueStaysVisible(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	pickerLoadPoolFn = func(context.Context, string) pickerPool { return nil }
	if !credentialPoolIsUsable(ctx, "opencode-go", true, false) || credentialPoolIsUsable(ctx, "opencode-go", false, false) {
		t.Fatal("raw pool presence must decide an empty pool")
	}
}

func bareCustomConfig(t *testing.T, yaml string) {
	t.Helper()
	writeConfigYAML(t, os.Getenv("ICEBATH_HOME"), yaml)
}

func TestBareCustomRowSurvivesSwitchingAway(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{}, true })
	bareCustomConfig(t, "model:\n  default: gpt-4o\n  provider: custom\n  base_url: https://www.ccsub.net/v1\n")
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "xai", CurrentModel: "grok-4", SkipCustomProviderProbe: true})
	row := pickerRow(rows, "custom")
	if row == nil || row["name"] != "Custom endpoint" || row["is_current"] != false || row["api_url"] != "https://www.ccsub.net/v1" ||
		row["source"] != "model-config" || !reflect.DeepEqual(rowModels(row), []string{"gpt-4o"}) {
		t.Fatalf("row %v", row)
	}
}

func TestBareCustomRowDefersToMatchingNamedEntry(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	bareCustomConfig(t, "model:\n  default: gpt-4o\n  provider: custom\n  base_url: https://www.ccsub.net/v1\n")
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "xai", CurrentModel: "grok-4", SkipCustomProviderProbe: true,
		CustomProviders: []any{map[string]any{"name": "my-endpoint", "base_url": "https://www.ccsub.net/v1", "model": "gpt-4o"}}})
	if pickerRow(rows, "custom") != nil || pickerRow(rows, "custom:my-endpoint") == nil {
		t.Fatalf("rows %v", rows)
	}
}

func TestBareCustomRowToleratesLegacyScalarModel(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	bareCustomConfig(t, "model: gpt-4o\n")
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "xai", CurrentModel: "grok-4", SkipCustomProviderProbe: true})
	if pickerRow(rows, "custom") != nil {
		t.Fatalf("rows %v", rows)
	}
}

// Current custom endpoint that answers only within the full five second budget
func slowEndpointRows(t *testing.T, fast *bool) map[string]any {
	t.Helper()
	ctx, _ := pickerTestHome(t, nil)
	saved := pickerFetchLiveModelsFn
	t.Cleanup(func() { pickerFetchLiveModelsFn = saved })
	pickerFetchLiveModelsFn = func(_ context.Context, _ any, _, _ string, _ bool, _ map[string]string, timeout time.Duration, _ string, _ bool) (CatalogRows, bool) {
		if timeout >= 5*time.Second {
			return CatalogRows{IDs: []string{"slow-discovered-model"}}, true
		}
		return CatalogRows{}, false
	}
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom", CurrentBaseURL: "http://127.0.0.1:9999/v1",
		CurrentModel: "kept-model", SkipCustomProviderProbe: true, ProbeCurrentCustomProvider: true, ForPicker: true, FastCustomProbe: fast})
	return pickerRow(rows, "custom")
}

func TestFullBudgetKeepsSlowEndpointModels(t *testing.T) {
	no := false
	if row := slowEndpointRows(t, &no); !anyListContains(row["models"].([]any), "slow-discovered-model") {
		t.Fatalf("row %v", row)
	}
}

func TestDefaultCallersKeepTheFastBudgetCoupling(t *testing.T) {
	if row := slowEndpointRows(t, nil); !reflect.DeepEqual(rowModels(row), []string{"kept-model"}) {
		t.Fatalf("row %v", row)
	}
}

func TestExplicitFastProbeStillCutsTheBudget(t *testing.T) {
	yes := true
	if row := slowEndpointRows(t, &yes); !reflect.DeepEqual(rowModels(row), []string{"kept-model"}) {
		t.Fatalf("row %v", row)
	}
}
