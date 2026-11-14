package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Native Ollama detection on with the tags probe answering the scripted catalogs in order
func nativePickerProbe(t *testing.T, answers ...[]string) {
	t.Helper()
	savedUse, savedHeaders, savedFetch := shouldUseOllamaNativeCatalogFn, getOllamaNativeHeadersFn, fetchOllamaLocalModelsFn
	t.Cleanup(func() {
		shouldUseOllamaNativeCatalogFn, getOllamaNativeHeadersFn, fetchOllamaLocalModelsFn = savedUse, savedHeaders, savedFetch
	})
	shouldUseOllamaNativeCatalogFn = func(context.Context, string, string, map[string]string) bool { return true }
	getOllamaNativeHeadersFn = func(context.Context, string, any) map[string]string { return map[string]string{} }
	call := 0
	fetchOllamaLocalModelsFn = func(context.Context, string, time.Duration, map[string]string) ([]string, bool) {
		if call >= len(answers) {
			t.Errorf("tags probe %d past the %d scripted answers", call+1, len(answers))
			return nil, false
		}
		answer := answers[call]
		call++
		return answer, answer != nil
	}
}

func pickerLive(ctx context.Context, apiKey any, url, provider string, cache bool) ([]string, bool) {
	rows, ok := fetchPickerLiveModels(ctx, apiKey, url, provider, false, nil, 5*time.Second, "", cache)
	return rows.IDs, ok
}

// Moves every cached catalog row back in time by the given span
func ageCachedRows(ctx context.Context, by time.Duration) {
	cache := loadProviderModelsCache(ctx)
	for key, raw := range cache {
		row, _ := asMap(raw)
		at, _ := floatOf(row["at"])
		row["at"] = at - by.Seconds()
		storeCacheEntry(ctx, key, row)
	}
}

func TestPickerNativeProbeFailureFallsBackToOpenaiCatalog(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{"fallback-model"}, true })
	nativePickerProbe(t, nil)
	if got, ok := pickerLive(ctx, "key", "http://127.0.0.1:11434/v1", "ollama", true); !ok || !reflect.DeepEqual(got, []string{"fallback-model"}) {
		t.Fatalf("got %v %v", got, ok)
	}
}

func TestPickerNativeCatalogIsAdmittedToTheSharedModelCache(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	nativePickerProbe(t, []string{"qwen3:8b"})
	url := "http://127.0.0.1:11434/v1"
	if got, ok := pickerLive(ctx, "sk-ollama", url, "custom", true); !ok || !reflect.DeepEqual(got, []string{"qwen3:8b"}) {
		t.Fatalf("got %v %v", got, ok)
	}
	noProbe, ok := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-ollama", BaseURL: url, CacheOnly: true, Timeout: 1500 * time.Millisecond})
	if !ok || !noProbe.Native || !reflect.DeepEqual(noProbe.IDs, []string{"qwen3:8b"}) {
		t.Fatalf("no probe read %+v %v", noProbe, ok)
	}
}

func TestPickerNativeCatalogSkipsCacheAdmissionWhenCacheIsOff(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	nativePickerProbe(t, []string{"qwen3:8b"})
	url := "http://127.0.0.1:11434/v1"
	if got, ok := pickerLive(ctx, nil, url, "custom", false); !ok || !reflect.DeepEqual(got, []string{"qwen3:8b"}) {
		t.Fatalf("got %v %v", got, ok)
	}
	if rows, ok := CachedFetchAPIModels(ctx, CachedFetchOptions{BaseURL: url, CacheOnly: true, Timeout: 1500 * time.Millisecond}); ok {
		t.Fatalf("admitted %+v", rows)
	}
}

func TestPickerNativeCatalogUsesTheShortNativeTTL(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	nativePickerProbe(t, []string{"qwen3:8b"})
	url := "http://127.0.0.1:11434/v1"
	if got, ok := pickerLive(ctx, "sk-ollama", url, "custom", true); !ok || !reflect.DeepEqual(got, []string{"qwen3:8b"}) {
		t.Fatalf("got %v %v", got, ok)
	}
	savedSpawn := spawnSWRRefreshFn
	t.Cleanup(func() { spawnSWRRefreshFn = savedSpawn })
	var refreshes []string
	spawnSWRRefreshFn = func(_ context.Context, key string, _ func() (map[string]any, bool)) {
		refreshes = append(refreshes, key)
	}
	ageCachedRows(ctx, ollamaLocalModelsCacheTTL+time.Second)
	if got, ok := pickerLive(ctx, "sk-ollama", url, "custom", true); !ok || !reflect.DeepEqual(got, []string{"qwen3:8b"}) {
		t.Fatalf("got %v %v", got, ok)
	}
	if len(refreshes) != 1 {
		t.Fatalf("row older than the native TTL must be revalidated, not fresh %v", refreshes)
	}
}

func TestPickerEmptyNativeCatalogIsNotStaleServed(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	nativePickerProbe(t, []string{}, []string{"back:latest"})
	url := "http://127.0.0.1:11434/v1"
	if got, ok := pickerLive(ctx, "sk-ollama", url, "custom", true); !ok || len(got) != 0 {
		t.Fatalf("got %v %v", got, ok)
	}
	ageCachedRows(ctx, providerModelsCacheTTL+100*time.Second)
	if got, ok := pickerLive(ctx, "sk-ollama", url, "custom", true); !ok || !reflect.DeepEqual(got, []string{"back:latest"}) {
		t.Fatalf("got %v %v", got, ok)
	}
}

func TestListAuthenticatedProvidersIncludesCustomProviders(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openai-codex", UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "Local (127.0.0.1:4141)", "base_url": "http://127.0.0.1:4141/v1", "model": "rotator-openrouter-coding"}}})
	row := pickerRow(rows, "custom:local-(127.0.0.1:4141)")
	if row == nil || row["name"] != "Local (127.0.0.1:4141)" || !reflect.DeepEqual(rowModels(row), []string{"rotator-openrouter-coding"}) || row["api_url"] != "http://127.0.0.1:4141/v1" {
		t.Fatalf("row %v", row)
	}
}

func TestListAuthenticatedProvidersNumericYamlProviderDictKey(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "2070", CurrentBaseURL: "http://192.168.1.10:8082/v1",
		CurrentModel: "Qwen3.5-9B-Q4_K_M.gguf", MaxModels: intPtr(0), SkipCustomProviderProbe: true, CustomProviders: []any{},
		UserProviders: map[string]any{"2070": map[string]any{"name": 2070, "base_url": "http://192.168.1.10:8082/v1", "model": "Qwen3.5-9B-Q4_K_M.gguf"}}})
	row := pickerRow(rows, "2070")
	if row == nil || row["name"] != "2070" || row["is_current"] != true {
		t.Fatalf("row %v", row)
	}
}

func TestListAuthenticatedProvidersNumericCustomProviderName(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "2070", CurrentBaseURL: "http://192.168.1.10:8082/v1",
		CurrentModel: "Qwen3.5-9B-Q4_K_M.gguf", MaxModels: intPtr(0), SkipCustomProviderProbe: true, UserProviders: map[string]any{},
		CustomProviders: []any{map[string]any{"name": 2070, "base_url": "http://192.168.1.10:8082/v1", "model": "Qwen3.5-9B-Q4_K_M.gguf"}}})
	for _, r := range rows {
		if r["name"] == "2070" || strings.Contains(r["slug"].(string), "2070") {
			return
		}
	}
	t.Fatalf("rows %v", rows)
}

func TestProvidersSingularModelDoesNotSuppressOllamaNativeDiscovery(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	savedFetch := fetchOllamaLocalModelsFn
	t.Cleanup(func() { fetchOllamaLocalModelsFn = savedFetch })
	fetchOllamaLocalModelsFn = func(context.Context, string, time.Duration, map[string]string) ([]string, bool) {
		return []string{"qwen3:latest", "llama3.2:latest"}, true
	}
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openai-codex", MaxModels: intPtr(50), CustomProviders: []any{},
		UserProviders: map[string]any{"ollama": map[string]any{"base_url": "http://localhost:11434/v1", "model": "qwen3:latest"}}})
	if row := pickerRow(rows, "ollama"); row == nil || !reflect.DeepEqual(rowModels(row), []string{"qwen3:latest", "llama3.2:latest"}) {
		t.Fatalf("row %v", row)
	}
}

func TestListSplitsCommaChainCustomProviderModel(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	chain := "deepseek-v4-flash, deepseek-v4-pro, glm-5.2, deepseek-v4-flash"
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openai-codex", UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "Volcengine Agent Plan", "base_url": "https://ark.cn-beijing.volces.com/api/v3", "model": chain}}})
	var matches []map[string]any
	for _, r := range rows {
		if r["name"] == "Volcengine Agent Plan" {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 || !reflect.DeepEqual(rowModels(matches[0]), []string{chain, "deepseek-v4-flash", "deepseek-v4-pro", "glm-5.2"}) || matches[0]["total_models"] != 4 {
		t.Fatalf("matches %v", matches)
	}
}

func TestListAuthenticatedProvidersCanSkipCustomProviderLiveProbe(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) {
		t.Errorf("unexpected probe")
		return nil, false
	})
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{UserProviders: map[string]any{}, SkipCustomProviderProbe: true,
		CustomProviders: []any{map[string]any{"name": "Slow Local", "base_url": "http://127.0.0.1:8080/v1", "api_key": "sk-local", "model": "local-model"}}})
	row := pickerRow(rows, "custom:slow-local")
	if row == nil || !reflect.DeepEqual(rowModels(row), []string{"local-model"}) || row["total_models"] != 1 {
		t.Fatalf("row %v", row)
	}
}

func TestResolveProviderFullFindsNamedCustomProvider(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	resolved := ResolveProviderFull(ctx, "custom:local-(127.0.0.1:4141)", map[string]any{},
		[]any{map[string]any{"name": "Local (127.0.0.1:4141)", "base_url": "http://127.0.0.1:4141/v1"}})
	if resolved == nil || resolved.ID != "custom:local-(127.0.0.1:4141)" || resolved.Name != "Local (127.0.0.1:4141)" ||
		resolved.BaseURL != "http://127.0.0.1:4141/v1" || resolved.Source != "user-config" {
		t.Fatalf("resolved %+v", resolved)
	}
}

func TestKeyedCustomProviderLegacyAliasesResolveToStableKey(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	custom := []any{map[string]any{"name": "Local Ollama", "provider_key": "local-127.0.0.1:11434", "base_url": "http://127.0.0.1:11434/v1"}}
	for _, requested := range []string{"Local Ollama", "local-ollama", "local-127.0.0.1:11434", "custom:local-ollama", "custom:local-127.0.0.1:11434"} {
		resolved := ResolveProviderFull(ctx, requested, map[string]any{}, custom)
		if resolved == nil || resolved.ID != "custom:local-127.0.0.1:11434" || resolved.Name != "Local Ollama" {
			t.Fatalf("%q resolved %+v", requested, resolved)
		}
	}
}

func TestKeyedCustomProviderBareCustomFallbackUsesStableKey(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	resolved := ResolveProviderFull(ctx, "custom", map[string]any{},
		[]any{map[string]any{"name": "Local Ollama", "provider_key": "local-127.0.0.1:11434", "base_url": "http://127.0.0.1:11434/v1"}})
	if resolved == nil || resolved.ID != "custom:local-127.0.0.1:11434" {
		t.Fatalf("resolved %+v", resolved)
	}
}

func TestPrefixedProviderKeyDoesNotAccumulateCustomPrefixes(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	custom := []any{map[string]any{"name": "Foo Relay", "provider_key": "custom:foo", "base_url": "https://foo.example/v1"}}
	for _, requested := range []string{"foo", "custom:foo", "custom:custom:foo"} {
		if resolved := ResolveProviderFull(ctx, requested, map[string]any{}, custom); resolved == nil || resolved.ID != "custom:foo" {
			t.Fatalf("%q resolved %+v", requested, resolved)
		}
	}
}

func TestListAuthenticatedProvidersIncludesActiveBareCustomEndpoint(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom", CurrentBaseURL: "https://www.ccsub.net/v1",
		CurrentModel: "gpt-4o", UserProviders: map[string]any{}, CustomProviders: []any{}, MaxModels: intPtr(50)})
	row := pickerRow(rows, "custom")
	if row == nil || row["is_current"] != true || row["is_user_defined"] != true || !reflect.DeepEqual(rowModels(row), []string{"gpt-4o"}) || row["api_url"] != "https://www.ccsub.net/v1" {
		t.Fatalf("row %v", row)
	}
}

func TestListAuthenticatedProvidersCanProbeActiveBareCustomEndpoint(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{"gpt-4o", "gpt-4o-mini"}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom", CurrentBaseURL: "https://www.ccsub.net/v1",
		CurrentModel: "gpt-4o", UserProviders: map[string]any{}, CustomProviders: []any{}, SkipCustomProviderProbe: true, ProbeCurrentCustomProvider: true})
	row := pickerRow(rows, "custom")
	if row == nil || row["is_current"] != true || !reflect.DeepEqual(rowModels(row), []string{"gpt-4o", "gpt-4o-mini"}) {
		t.Fatalf("row %v", row)
	}
}

func rowsNamed(rows []map[string]any, name string) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r["name"] == name {
			out = append(out, r)
		}
	}
	return out
}

func userDefinedRows(rows []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r["is_user_defined"] == true {
			out = append(out, r)
		}
	}
	return out
}

func callURLs(calls *[]pickerFetchCall) []string {
	out := []string{}
	for _, c := range *calls {
		out = append(out, c.baseURL)
	}
	return out
}

func TestIsAggregatorRecognizesNamedCustomProvider(t *testing.T) {
	if !IsAggregator("custom:hpc-ai") || !IsAggregator("custom:litellm") {
		t.Fatal("named custom providers are aggregators")
	}
}

func TestListGroupsSameNameCustomProvidersIntoOneRow(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{
			map[string]any{"name": "Ollama Cloud", "base_url": "https://ollama.com/v1", "model": "qwen3-coder:480b-cloud"},
			map[string]any{"name": "Ollama Cloud", "base_url": "https://ollama.com/v1", "model": "glm-5.1:cloud"},
			map[string]any{"name": "Ollama Cloud", "base_url": "https://ollama.com/v1", "model": "kimi-k2.5"},
			map[string]any{"name": "Ollama Cloud", "base_url": "https://ollama.com/v1", "model": "minimax-m2.7:cloud"},
			map[string]any{"name": "Moonshot", "base_url": "https://api.moonshot.ai/v1", "model": "kimi-k2-thinking"}}})
	ollama := rowsNamed(rows, "Ollama Cloud")
	if len(ollama) != 1 || !reflect.DeepEqual(rowModels(ollama[0]), []string{"qwen3-coder:480b-cloud", "glm-5.1:cloud", "kimi-k2.5", "minimax-m2.7:cloud"}) || ollama[0]["total_models"] != 4 {
		t.Fatalf("Expected 1 Ollama Cloud row, got %v", ollama)
	}
	if moonshot := rowsNamed(rows, "Moonshot"); len(moonshot) != 1 || !reflect.DeepEqual(rowModels(moonshot[0]), []string{"kimi-k2-thinking"}) {
		t.Fatalf("moonshot %v", moonshot)
	}
}

func TestListDeduplicatesSameModelInGroup(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{}, true })
	entry := func(model string) map[string]any {
		return map[string]any{"name": "MyProvider", "base_url": "http://localhost:11434/v1", "model": model, "discover_models": false}
	}
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{entry("llama3"), entry("llama3"), entry("mistral")}})
	mine := rowsNamed(rows, "MyProvider")
	if len(mine) != 1 || !reflect.DeepEqual(rowModels(mine[0]), []string{"llama3", "mistral"}) || mine[0]["total_models"] != 2 {
		t.Fatalf("rows %v", mine)
	}
}

func TestCustomProviderNoKeySingularModelStillProbesLiveModels(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{"llama3", "mistral", "qwen3-coder"}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openai-codex", UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "Local llama.cpp", "base_url": "http://localhost:8080/v1", "model": "llama3"}}})
	if len(*calls) != 1 || (*calls)[0].apiKey != "" || (*calls)[0].baseURL != "http://localhost:8080/v1" {
		t.Fatalf("calls %+v", *calls)
	}
	row := rowsNamed(rows, "Local llama.cpp")[0]
	if !reflect.DeepEqual(rowModels(row), []string{"llama3", "mistral", "qwen3-coder"}) || row["total_models"] != 3 {
		t.Fatalf("row %v", row)
	}
}

func TestCustomProviderGroupExplicitDuplicateSkipsProbe(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{"unexpected-live-model"}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:local-ollama", UserProviders: map[string]any{},
		CustomProviders: []any{
			map[string]any{"name": "Local Ollama", "base_url": "http://localhost:11434/v1", "model": "llama3"},
			map[string]any{"name": "Local Ollama", "base_url": "http://localhost:11434/v1", "models": []any{"llama3"}}}})
	row := rowsNamed(rows, "Local Ollama")[0]
	if len(*calls) != 0 || !reflect.DeepEqual(rowModels(row), []string{"llama3"}) {
		t.Fatalf("calls %+v row %v", *calls, row)
	}
}

func TestCustomProviderCurrentOnlyProbeRespectsExplicitCatalog(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{"live-a", "live-b"}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:active", CurrentBaseURL: "http://active.local/v1",
		UserProviders: map[string]any{}, SkipCustomProviderProbe: true, ProbeCurrentCustomProvider: true,
		CustomProviders: []any{
			map[string]any{"name": "Active", "base_url": "http://active.local/v1", "model": "seed"},
			map[string]any{"name": "Offline", "base_url": "http://offline.local/v1", "model": "offline-seed"},
			map[string]any{"name": "Static", "base_url": "http://static.local/v1", "model": "only", "models": []any{"only"}}}})
	if got := callURLs(calls); !reflect.DeepEqual(got, []string{"http://active.local/v1"}) {
		t.Fatalf("calls %v", got)
	}
	byName := map[string][]string{}
	for _, r := range userDefinedRows(rows) {
		byName[r["name"].(string)] = rowModels(r)
	}
	if !reflect.DeepEqual(byName["Active"], []string{"live-a", "live-b"}) || !reflect.DeepEqual(byName["Offline"], []string{"offline-seed"}) || !reflect.DeepEqual(byName["Static"], []string{"only"}) {
		t.Fatalf("rows %v", byName)
	}
}

func TestCustomProviderCurrentExplicitCatalogSkipsProbe(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{"unexpected-live-model"}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:static", CurrentBaseURL: "http://static.local/v1",
		UserProviders: map[string]any{}, SkipCustomProviderProbe: true, ProbeCurrentCustomProvider: true,
		CustomProviders: []any{map[string]any{"name": "Static", "base_url": "http://static.local/v1", "model": "only", "models": []any{"only"}}}})
	row := rowsNamed(rows, "Static")[0]
	if len(*calls) != 0 || row["is_current"] != true || !reflect.DeepEqual(rowModels(row), []string{"only"}) {
		t.Fatalf("calls %+v row %v", *calls, row)
	}
}

func TestCustomProviderEmptyExplicitListAllowsProbe(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{"live-a", "live-b"}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:local", UserProviders: map[string]any{},
		CustomProviders: []any{map[string]any{"name": "Local", "base_url": "http://local.test/v1", "model": "seed", "models": []any{}}}})
	row := rowsNamed(rows, "Local")[0]
	if got := callURLs(calls); !reflect.DeepEqual(got, []string{"http://local.test/v1"}) || !reflect.DeepEqual(rowModels(row), []string{"live-a", "live-b"}) {
		t.Fatalf("calls %v row %v", got, row)
	}
}

func TestListEnumeratesDictFormatModelsAlongsideDefault(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openai-codex", UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "DeepSeek", "base_url": "https://api.deepseek.com", "api_mode": "chat_completions", "model": "deepseek-chat",
			"models": map[string]any{"deepseek-chat": map[string]any{"context_length": 128000}, "deepseek-reasoner": map[string]any{"context_length": 128000}}}}})
	ds := rowsNamed(rows, "DeepSeek")
	if len(ds) != 1 || !reflect.DeepEqual(rowModels(ds[0]), []string{"deepseek-chat", "deepseek-reasoner"}) || ds[0]["total_models"] != 2 {
		t.Fatalf("rows %v", ds)
	}
}

func TestListEnumeratesDictFormatModelsWithoutSingularModel(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openai-codex", UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "Thor", "base_url": "http://thor.lab:8337/v1", "models": map[string]any{
			"gemma-4-26B-A4B-it-MXFP4_MOE": map[string]any{"context_length": 262144}, "Qwen3.5-35B-A3B-MXFP4_MOE": map[string]any{"context_length": 262144},
			"gemma-4-31B-it-Q4_K_M": map[string]any{"context_length": 262144}}}}})
	thor := rowsNamed(rows, "Thor")
	want := sortedStrings([]string{"gemma-4-26B-A4B-it-MXFP4_MOE", "Qwen3.5-35B-A3B-MXFP4_MOE", "gemma-4-31B-it-Q4_K_M"})
	if len(thor) != 1 || !reflect.DeepEqual(sortedStrings(rowModels(thor[0])), want) || thor[0]["total_models"] != 3 {
		t.Fatalf("rows %v", thor)
	}
}

func TestListAuthenticatedProvidersGroupsSameEndpoint(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	entry := func(name, model string) map[string]any {
		return map[string]any{"name": name, "base_url": "http://localhost:11434/v1", "api_key": "ollama", "model": model, "discover_models": false}
	}
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom", CurrentBaseURL: "http://localhost:11434/v1",
		UserProviders: map[string]any{}, MaxModels: intPtr(50), SkipCustomProviderProbe: true,
		CustomProviders: []any{entry("Ollama — MiniMax M2.7", "minimax-m2.7"), entry("Ollama — GLM 5.1", "glm-5.1"), entry("Ollama — Qwen3-coder", "qwen3-coder")}})
	groups := userDefinedRows(rows)
	if len(groups) != 1 {
		t.Fatalf("Expected 1 group for shared endpoint, got %v", groups)
	}
	if !reflect.DeepEqual(sortedStrings(rowModels(groups[0])), []string{"glm-5.1", "minimax-m2.7", "qwen3-coder"}) || groups[0]["total_models"] != 3 || groups[0]["name"] != "Ollama" {
		t.Fatalf("group %v", groups[0])
	}
}

func TestListAuthenticatedProvidersCurrentEndpointUsesCurrentSlug(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:ollama", CurrentBaseURL: "http://localhost:11434/v1",
		UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "Ollama — GLM 5.1", "base_url": "http://localhost:11434/v1", "api_key": "ollama", "model": "glm-5.1"}}})
	matches := userDefinedRows(rows)
	if len(matches) != 1 || matches[0]["slug"] != "custom:ollama" || matches[0]["is_current"] != true {
		t.Fatalf("matches %v", matches)
	}
}

func TestPickerEndpointAuthorizationOverridesInferredBearer(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	savedUse, savedFetch := shouldUseOllamaNativeCatalogFn, fetchOllamaLocalModelsFn
	t.Cleanup(func() { shouldUseOllamaNativeCatalogFn, fetchOllamaLocalModelsFn = savedUse, savedFetch })
	shouldUseOllamaNativeCatalogFn = func(context.Context, string, string, map[string]string) bool { return true }
	captured := map[string]string{}
	fetchOllamaLocalModelsFn = func(_ context.Context, _ string, _ time.Duration, headers map[string]string) ([]string, bool) {
		for k, v := range headers {
			captured[k] = v
		}
		return []string{"model-a"}, true
	}
	rows, ok := fetchPickerLiveModels(ctx, "endpoint-key", "http://127.0.0.1:11434/v1", "ollama", false, map[string]string{"authorization": "Api-Key explicit"}, 5*time.Second, "", true)
	if !ok || !reflect.DeepEqual(rows.IDs, []string{"model-a"}) {
		t.Fatalf("rows %+v %v", rows, ok)
	}
	auth := map[string]string{}
	for k, v := range captured {
		if strings.ToLower(k) == "authorization" {
			auth[k] = v
		}
	}
	if !reflect.DeepEqual(auth, map[string]string{"authorization": "Api-Key explicit"}) {
		t.Fatalf("auth headers %v", auth)
	}
}

func TestListAuthenticatedProvidersBareCustomSlugRecovers(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom", CurrentBaseURL: "http://localhost:11434/v1",
		UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "Ollama — GLM 5.1", "base_url": "http://localhost:11434/v1", "api_key": "ollama", "model": "glm-5.1"}}})
	matches := userDefinedRows(rows)
	if len(matches) != 1 || matches[0]["slug"] != "custom:ollama" || matches[0]["is_current"] != true {
		t.Fatalf("matches %v", matches)
	}
}

func TestCompatibleKeyedProviderUsesStableKeyAndAcceptsLegacyCurrentName(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:local-ollama", UserProviders: map[string]any{}, MaxModels: intPtr(50), SkipCustomProviderProbe: true,
		CustomProviders: []any{map[string]any{"name": "Local Ollama", "provider_key": "local-127.0.0.1:11434", "base_url": "http://127.0.0.1:11434/v1", "model": "qwen3.5:9b"}}})
	row := userDefinedRows(rows)[0]
	if row["slug"] != "custom:local-127.0.0.1:11434" || row["is_current"] != true {
		t.Fatalf("row %v", row)
	}
}

func TestUserProviderRowRecognizesStableCustomKeyAsCurrent(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:local-127.0.0.1:11434", CustomProviders: []any{}, MaxModels: intPtr(50), SkipCustomProviderProbe: true,
		UserProviders: map[string]any{"local-127.0.0.1:11434": map[string]any{"name": "Local Ollama", "base_url": "http://127.0.0.1:11434/v1", "default_model": "qwen3.5:9b"}}})
	row := userDefinedRows(rows)[0]
	if row["slug"] != "local-127.0.0.1:11434" || row["is_current"] != true {
		t.Fatalf("row %v", row)
	}
}

func TestListAuthenticatedProvidersDistinctEndpointsStaySeparate(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{UserProviders: map[string]any{}, MaxModels: intPtr(50), SkipCustomProviderProbe: true,
		CustomProviders: []any{
			map[string]any{"name": "Ollama — GLM 5.1", "base_url": "http://localhost:11434/v1", "api_key": "ollama", "model": "glm-5.1"},
			map[string]any{"name": "Moonshot", "base_url": "https://api.moonshot.cn/v1", "api_key": "sk-m", "model": "moonshot-v1"},
			map[string]any{"name": "Ollama — Qwen3-coder", "base_url": "http://localhost:11434/v1", "api_key": "ollama", "model": "qwen3-coder"}}})
	groups := userDefinedRows(rows)
	if len(groups) != 2 {
		t.Fatalf("groups %v", groups)
	}
	ollama, moonshot := rowsNamed(groups, "Ollama"), rowsNamed(groups, "Moonshot")
	if len(ollama) != 1 || !reflect.DeepEqual(sortedStrings(rowModels(ollama[0])), []string{"glm-5.1", "qwen3-coder"}) || len(moonshot) != 1 || !reflect.DeepEqual(rowModels(moonshot[0]), []string{"moonshot-v1"}) {
		t.Fatalf("groups %v", groups)
	}
}

func TestListAuthenticatedProvidersSameUrlDifferentKeysDisambiguated(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{
			map[string]any{"name": "OpenAI — key A", "base_url": "https://api.openai.com/v1", "api_key": "sk-AAA", "model": "gpt-5.4"},
			map[string]any{"name": "OpenAI — key B", "base_url": "https://api.openai.com/v1", "api_key": "sk-BBB", "model": "gpt-4.6"}}})
	groups := userDefinedRows(rows)
	slugs := []string{}
	models := map[string][]string{}
	for _, g := range groups {
		slugs = append(slugs, g["slug"].(string))
		models[g["slug"].(string)] = rowModels(g)
	}
	if len(groups) != 2 || !reflect.DeepEqual(sortedStrings(slugs), []string{"custom:openai", "custom:openai-2"}) ||
		!reflect.DeepEqual(models["custom:openai"], []string{"gpt-5.4"}) || !reflect.DeepEqual(models["custom:openai-2"], []string{"gpt-4.6"}) {
		t.Fatalf("groups %v", groups)
	}
}

func TestListAuthenticatedProvidersSameUrlDifferentKeyEnvAndApiModeStaySeparate(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:gpt", CurrentBaseURL: "https://gateway.example.com", UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{
			map[string]any{"name": "gpt", "base_url": "https://gateway.example.com", "key_env": "GPT_KEY", "api_mode": "codex_responses", "model": "gpt-5.5"},
			map[string]any{"name": "claude", "base_url": "https://gateway.example.com", "key_env": "CLAUDE_KEY", "api_mode": "anthropic_messages", "model": "claude-opus-4-8"}}})
	bySlug := map[string]map[string]any{}
	for _, g := range userDefinedRows(rows) {
		bySlug[g["slug"].(string)] = g
	}
	if len(bySlug) != 2 || bySlug["custom:gpt"] == nil || bySlug["custom:claude"] == nil {
		t.Fatalf("rows %v", bySlug)
	}
	if !reflect.DeepEqual(rowModels(bySlug["custom:gpt"]), []string{"gpt-5.5"}) || !reflect.DeepEqual(rowModels(bySlug["custom:claude"]), []string{"claude-opus-4-8"}) ||
		bySlug["custom:gpt"]["is_current"] != true || bySlug["custom:claude"]["is_current"] != false {
		t.Fatalf("rows %v", bySlug)
	}
}

// LM Studio catalog fetch replaced for one test with the env redirects cleared
func fakeLMStudio(t *testing.T, fetch func(apiKey any, baseURL string) []string) *[]string {
	t.Helper()
	t.Setenv("LM_BASE_URL", "")
	t.Setenv("LM_API_KEY", "")
	saved := pickerLMStudioFn
	t.Cleanup(func() { pickerLMStudioFn = saved })
	captured := &[]string{}
	pickerLMStudioFn = func(_ context.Context, apiKey any, baseURL string, _ time.Duration) ([]string, error) {
		*captured = append(*captured, baseURL)
		return fetch(apiKey, baseURL), nil
	}
	return captured
}

func TestLmstudioPickerProbesActiveConfigBaseUrl(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	captured := fakeLMStudio(t, func(any, string) []string { return []string{"qwen/qwen3-coder-30b"} })
	mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "lmstudio", CurrentBaseURL: "http://192.168.1.10:1234/v1", CurrentModel: "qwen/qwen3-coder-30b"})
	if !reflect.DeepEqual(*captured, []string{"http://192.168.1.10:1234/v1"}) {
		t.Fatalf("probed %v", *captured)
	}
}

func TestLmstudioPickerLmBaseUrlEnvWinsOverActiveConfig(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	captured := fakeLMStudio(t, func(any, string) []string { return []string{} })
	t.Setenv("LM_BASE_URL", "http://override.local:9999/v1")
	mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "lmstudio", CurrentBaseURL: "http://192.168.1.10:1234/v1"})
	if !reflect.DeepEqual(*captured, []string{"http://override.local:9999/v1"}) {
		t.Fatalf("probed %v", *captured)
	}
}

func TestLmstudioPickerSkipsProbeWhenNotConfigured(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	captured := fakeLMStudio(t, func(any, string) []string { return []string{} })
	mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", CurrentBaseURL: "https://openrouter.ai/api/v1"})
	if len(*captured) != 0 {
		t.Fatalf("probed %v", *captured)
	}
}

func TestLmstudioBareProvidersBlockDoesNotHideLiveCatalog(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	live := []string{"model-a", "model-b", "model-c"}
	fakeLMStudio(t, func(any, string) []string { return append([]string{}, live...) })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "lmstudio", CurrentBaseURL: "http://127.0.0.1:1234/v1", CurrentModel: "model-a",
		UserProviders: map[string]any{"lmstudio": map[string]any{"request_timeout_seconds": 86400, "stale_timeout_seconds": 86400}}})
	var matches []map[string]any
	for _, r := range rows {
		if r["slug"] == "lmstudio" {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 || !reflect.DeepEqual(sortedStrings(rowModels(matches[0])), sortedStrings(live)) || matches[0]["total_models"] != 3 || matches[0]["source"] != "hermes" {
		t.Fatalf("rows %v", matches)
	}
}

func TestLmstudioProvidersBlockWithExplicitEndpointStillUsesSection3(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	fakeLMStudio(t, func(any, string) []string { return []string{"should-not-be-used"} })
	saved := pickerDiscoverEndpointFn
	t.Cleanup(func() { pickerDiscoverEndpointFn = saved })
	pickerDiscoverEndpointFn = func(context.Context, any, string, string, bool, map[string]string, string, bool, bool, bool) ([]string, bool, bool) {
		return []string{"remote-model"}, true, false
	}
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "lmstudio", CurrentBaseURL: "http://remote-box:1234/v1", CurrentModel: "remote-model",
		UserProviders: map[string]any{"lmstudio": map[string]any{"base_url": "http://remote-box:1234/v1", "discover_models": true}}})
	var matches []map[string]any
	for _, r := range rows {
		if r["slug"] == "lmstudio" {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 || matches[0]["is_user_defined"] != true || !reflect.DeepEqual(rowModels(matches[0]), []string{"remote-model"}) {
		t.Fatalf("rows %v", matches)
	}
}

func rowWithAPIURL(rows []map[string]any, url string) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r["api_url"] == url {
			out = append(out, r)
		}
	}
	return out
}

func TestCustomProvidersUsesLiveModelsForMultiModelEndpoint(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) {
		return []string{"gateway-model-a", "gateway-model-b", "gateway-model-c"}, true
	})
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", CurrentBaseURL: "https://openrouter.ai/api/v1", MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "my-gateway", "api_key": "sk-gateway-key", "base_url": "https://gateway.example.com/v1", "model": "gateway-model-a",
			"models": map[string]any{"gateway-model-a": map[string]any{"context_length": 128000}, "gateway-model-b": map[string]any{"context_length": 128000}}}}})
	gateway := rowWithAPIURL(rows, "https://gateway.example.com/v1")
	if len(gateway) == 0 {
		t.Fatalf("Custom provider group not found in results %v", rows)
	}
	if len(*calls) != 1 || (*calls)[0].apiKey != "sk-gateway-key" || (*calls)[0].baseURL != "https://gateway.example.com/v1" {
		t.Fatalf("fetch_api_models must be called with the custom provider's credentials %+v", *calls)
	}
	if !reflect.DeepEqual(rowModels(gateway[0]), []string{"gateway-model-a", "gateway-model-b", "gateway-model-c"}) || gateway[0]["total_models"] != 3 {
		t.Fatalf("Live models must replace the static subset %v", gateway[0])
	}
}

func TestSameEndpointDifferentExtraHeadersNotCollapsed(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	type probe struct {
		apiKey, baseURL string
		headers         map[string]string
	}
	var calls []probe
	fetchAPIModelsFn = func(_ context.Context, apiKey, baseURL string, _ time.Duration, _ string, headers map[string]string) ([]string, bool) {
		calls = append(calls, probe{apiKey, baseURL, headers})
		tenant := "none"
		if v, ok := headers["X-Tenant"]; ok {
			tenant = v
		}
		return []string{"model-" + tenant}, true
	}
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", CurrentBaseURL: "https://openrouter.ai/api/v1", MaxModels: intPtr(50),
		CustomProviders: []any{
			map[string]any{"name": "Proxy Tenant A", "api_key": "shared-key", "base_url": "http://localhost:8081/v1", "extra_headers": map[string]any{"X-Tenant": "a"}},
			map[string]any{"name": "Proxy Tenant B", "api_key": "shared-key", "base_url": "http://localhost:8081/v1", "extra_headers": map[string]any{"X-Tenant": "b"}}}})
	tenantRows := rowWithAPIURL(rows, "http://localhost:8081/v1")
	if len(tenantRows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", len(tenantRows), tenantRows)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if c.apiKey == "shared-key" && c.baseURL == "http://localhost:8081/v1" && len(c.headers) == 1 {
			seen[c.headers["X-Tenant"]] = true
		}
	}
	modelsByRow := map[string]bool{}
	for _, r := range tenantRows {
		modelsByRow[strings.Join(rowModels(r), ",")] = true
	}
	if !seen["a"] || !seen["b"] || !reflect.DeepEqual(modelsByRow, map[string]bool{"model-a": true, "model-b": true}) {
		t.Fatalf("calls %+v rows %v", calls, modelsByRow)
	}
}

func TestResolveCustomProviderPassesKeyEnv(t *testing.T) {
	resolved := ResolveCustomProvider("custom:token-plan", []any{map[string]any{"name": "token-plan", "base_url": "https://token-plan-sgp.xiaomimimo.com/v1",
		"key_env": "XIAOMI_MIMO_API_KEY", "model": "mimo-v2-pro"}})
	if resolved == nil || !reflect.DeepEqual(resolved.KeyEnv, []string{"XIAOMI_MIMO_API_KEY"}) || resolved.BaseURL != "https://token-plan-sgp.xiaomimimo.com/v1" {
		t.Fatalf("resolved %+v", resolved)
	}
}

type discoveredSave struct {
	apiURL   string
	modelIDs []string
	apiMode  string
	headers  map[string]string
	identity string
}

// Records every discovered catalog save instead of writing config
func recordDiscoveredSaves(t *testing.T) *[]discoveredSave {
	t.Helper()
	saved := pickerSaveDiscoveredFn
	t.Cleanup(func() { pickerSaveDiscoveredFn = saved })
	calls := &[]discoveredSave{}
	pickerSaveDiscoveredFn = func(_ context.Context, apiURL string, modelIDs []string, apiMode string, headers map[string]string, identity *string) {
		rec := discoveredSave{apiURL: apiURL, modelIDs: modelIDs, apiMode: apiMode, headers: headers}
		if identity != nil {
			rec.identity = *identity
		}
		*calls = append(*calls, rec)
	}
	return calls
}

func TestDiscoveredModelsAutoSavedToCache(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) {
		return []string{"discovered-a", "discovered-b", "discovered-c"}, true
	})
	saves := recordDiscoveredSaves(t)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "my-gateway", CurrentBaseURL: "https://gateway.example.com/v1", MaxModels: intPtr(50),
		CustomProviders: []any{map[string]any{"name": "my-gateway", "api_key": "***", "base_url": "https://gateway.example.com/v1", "discover_models": true,
			"model": "only-model", "models": map[string]any{"only-model": map[string]any{"context_length": 128000}}}}})
	if len(*saves) != 1 {
		t.Fatalf("_save_discovered_models_to_config must be called after a successful probe %+v", *saves)
	}
	if (*saves)[0].apiURL != "https://gateway.example.com/v1" || !reflect.DeepEqual((*saves)[0].modelIDs, []string{"discovered-a", "discovered-b", "discovered-c"}) {
		t.Fatalf("save %+v", (*saves)[0])
	}
	gateway := rowWithAPIURL(rows, "https://gateway.example.com/v1")
	if len(gateway) == 0 || !reflect.DeepEqual(rowModels(gateway[0]), []string{"discovered-a", "discovered-b", "discovered-c"}) {
		t.Fatalf("rows %v", gateway)
	}
}

func TestSaveDiscoveredModelsPreservesDictForm(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	h := os.Getenv("ICEBATH_HOME")
	writeConfigYAML(t, h, "custom_providers:\n  - name: my-gateway\n    base_url: https://gateway.example.com/v1\n    models:\n      configured-model:\n        context_length: 8192\n")
	before, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	saveDiscoveredModelsToConfig(ctx, "https://gateway.example.com/v1", []string{"configured-model", "discovered-model"}, "", nil, nil)
	after, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("Dict-form models must not be replaced with a flat list\n%s", after)
	}
}

func TestSharedUrlDifferentDisplayNamesAreSeparateRows(t *testing.T) {
	ctx, _ := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", CurrentBaseURL: "https://openrouter.ai/api/v1", UserProviders: map[string]any{}, MaxModels: intPtr(50),
		CustomProviders: []any{
			map[string]any{"name": "Cerebras", "base_url": "https://proxy.example.com/v1", "api_key": "proxy-key", "model": "llama-4-scout"},
			map[string]any{"name": "Groq", "base_url": "https://proxy.example.com/v1", "api_key": "proxy-key", "model": "llama-4-scout"},
			map[string]any{"name": "Perplexity", "base_url": "https://proxy.example.com/v1", "api_key": "proxy-key", "model": "sonar-pro"}}})
	names := []string{}
	byName := map[string][]string{}
	for _, r := range userDefinedRows(rows) {
		names = append(names, r["name"].(string))
		byName[r["name"].(string)] = rowModels(r)
	}
	if !reflect.DeepEqual(sortedStrings(names), []string{"Cerebras", "Groq", "Perplexity"}) {
		t.Fatalf("expected three separate rows, got %v", names)
	}
	if !reflect.DeepEqual(byName["Cerebras"], []string{"llama-4-scout"}) || !reflect.DeepEqual(byName["Groq"], []string{"llama-4-scout"}) || !reflect.DeepEqual(byName["Perplexity"], []string{"sonar-pro"}) {
		t.Fatalf("rows %v", byName)
	}
}

func TestExcludedProvidersHidesBuiltinRow(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	opts := AuthenticatedProvidersOptions{CurrentProvider: "openrouter", CurrentBaseURL: "https://openrouter.ai/api/v1", UserProviders: map[string]any{}, CustomProviders: []any{}, MaxModels: intPtr(50)}
	if pickerRow(mustListProviders(t, ctx, opts), "openrouter") == nil {
		t.Fatal("sanity: openrouter row must appear when OPENROUTER_API_KEY is set")
	}
	opts.ExcludedProviders = []string{"openrouter"}
	if pickerRow(mustListProviders(t, ctx, opts), "openrouter") != nil {
		t.Fatal("excluded_providers=['openrouter'] must hide the openrouter row")
	}
}

func TestCustomProviderContextLengthModelsDictStillProbes(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) {
		return []string{"qwen3.6:35b-mlx", "gemma4:31b", "llama3"}, true
	})
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:local-ollama", CurrentBaseURL: "http://localhost:11434/v1", UserProviders: map[string]any{},
		SkipCustomProviderProbe: true, ProbeCurrentCustomProvider: true,
		CustomProviders: []any{map[string]any{"name": "Local Ollama", "base_url": "http://localhost:11434/v1", "model": "qwen3.6:35b-mlx",
			"models": map[string]any{"qwen3.6:35b-mlx": map[string]any{"context_length": 32768}}}}})
	if len(*calls) != 1 || (*calls)[0].apiKey != "" || (*calls)[0].baseURL != "http://localhost:11434/v1" {
		t.Fatalf("calls %+v", *calls)
	}
	row := rowsNamed(rows, "Local Ollama")[0]
	if !reflect.DeepEqual(rowModels(row), []string{"qwen3.6:35b-mlx", "gemma4:31b", "llama3"}) || row["total_models"] != 3 {
		t.Fatalf("row %v", row)
	}
}

const (
	localEndpoint  = "http://127.0.0.1:8000/v1"
	sharedProxyURL = "https://proxy.example.com/v1"
)

func localCatalog() []string {
	out := []string{}
	for i := 1; i <= 8; i++ {
		out = append(out, fmt.Sprintf("omlx-model-%d", i))
	}
	return out
}

// Puts the models on disk for one endpoint under the fingerprint the picker reads it with
func seedEndpointCache(t *testing.T, url string, apiKey any, models []string, age time.Duration) {
	t.Helper()
	fp := customEndpointFingerprintFn(apiKey, "", nil)
	cache := map[string]any{"custom:" + url + "#" + fp: map[string]any{"fp": fp, "at": float64(time.Now().UnixNano())/1e9 - age.Seconds(), "models": stringsToAny(models)}}
	saved := loadProviderModelsCacheFn
	t.Cleanup(func() { loadProviderModelsCacheFn = saved })
	loadProviderModelsCacheFn = func(context.Context) map[string]any { return cache }
}

func seedCustomModelCache(t *testing.T, models []string) {
	t.Helper()
	seedEndpointCache(t, localEndpoint, "", models, 10*time.Second)
}

// GUI picker open without live probing, the row at the URL and every base URL a live fetch tried
func noProbeRow(t *testing.T, ctx context.Context, url string, opts AuthenticatedProvidersOptions) (map[string]any, []string) {
	t.Helper()
	fetched := []string{}
	fetchAPIModelsFn = func(_ context.Context, _, baseURL string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
		fetched = append(fetched, baseURL)
		return []string{"should-not-be-reached"}, true
	}
	if opts.CurrentProvider == "" {
		opts.CurrentProvider = "nous"
	}
	if opts.UserProviders == nil {
		opts.UserProviders = map[string]any{}
	}
	if opts.CustomProviders == nil {
		opts.CustomProviders = []any{}
	}
	opts.ForPicker, opts.Refresh, opts.SkipCustomProviderProbe, opts.ProbeCurrentCustomProvider = true, false, true, true
	for _, r := range mustListProviders(t, ctx, opts) {
		if strings.Contains(fmt.Sprint(r["api_url"]), url) {
			return r, fetched
		}
	}
	return nil, fetched
}

func localEntry(model string) map[string]any {
	return map[string]any{"name": "Local (127.0.0.1:8000)", "base_url": localEndpoint, "model": model}
}

// Model IDs of a saved models block, list items or the sorted keys of a mapping
func listedModelIDs(t *testing.T, value any) []string {
	t.Helper()
	out := []string{}
	switch v := value.(type) {
	case []any:
		for _, it := range v {
			out = append(out, fmt.Sprint(it))
		}
	case map[string]any:
		for k := range v {
			out = append(out, k)
		}
		sort.Strings(out)
	default:
		t.Fatalf("models is %T", value)
	}
	return out
}

func TestModelFlowNamedCustomPersistsDiscoveredModels(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	r := &ModelFlowRun{Ctx: env.Ctx, Env: env, Out: io.Discard, Config: map[string]any{}}
	swapFlowSeam(t, &flowFetchAPIModelsFn, func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		return []string{"discovered-a", "discovered-b", "discovered-c"}, true
	})
	swapFlowSeam(t, &flowRadiolistFn, func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error) { return 0, nil })
	saves := recordDiscoveredSaves(t)
	err := modelFlowNamedCustom(r, map[string]any{"name": "Dragomes", "base_url": "http://example.com/v1", "api_mode": "anthropic_messages",
		"extra_headers": map[string]any{"X-Tenant": "dragomes"}, "api_key": "sk-test", "key_env": "", "model": "MiniMax-M3", "provider_key": "",
		"discover_models": true, "models": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	want := []discoveredSave{{apiURL: "http://example.com/v1", modelIDs: []string{"discovered-a", "discovered-b", "discovered-c"},
		apiMode: "anthropic_messages", headers: map[string]string{"X-Tenant": "dragomes"}, identity: "sk-test"}}
	if !reflect.DeepEqual(*saves, want) {
		t.Fatalf("_model_flow_named_custom must persist each live catalog with its base URL, API mode, and endpoint headers %+v", *saves)
	}
}

func TestCustomProviderDictModelsPinRequiresDiscoverFalse(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) { return []string{"unexpected-live-model"}, true })
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom:local-ollama", UserProviders: map[string]any{},
		CustomProviders: []any{map[string]any{"name": "Local Ollama", "base_url": "http://localhost:11434/v1", "model": "llama3",
			"models": map[string]any{"llama3": map[string]any{}}, "discover_models": false}}})
	row := rowsNamed(rows, "Local Ollama")[0]
	if len(*calls) != 0 || !reflect.DeepEqual(rowModels(row), []string{"llama3"}) {
		t.Fatalf("calls %+v row %v", *calls, row)
	}
}

func TestNoProbeOpenServesCachedCatalogForCustomProvider(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	seedCustomModelCache(t, localCatalog())
	row, fetched := noProbeRow(t, ctx, localEndpoint, AuthenticatedProvidersOptions{CustomProviders: []any{localEntry("omlx-model-1")}})
	if row == nil || row["is_current"] != false || !reflect.DeepEqual(rowModels(row), localCatalog()) || row["total_models"] != 8 || len(fetched) != 0 {
		t.Fatalf("row %v fetched %v", row, fetched)
	}
}

func TestNoProbeOpenServesCachedCatalogForUserProvider(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	seedCustomModelCache(t, localCatalog())
	row, fetched := noProbeRow(t, ctx, localEndpoint, AuthenticatedProvidersOptions{UserProviders: map[string]any{
		"local-8000": map[string]any{"name": "Local (127.0.0.1:8000)", "base_url": localEndpoint, "default_model": "omlx-model-1"}}})
	if row == nil || !reflect.DeepEqual(rowModels(row), localCatalog()) || len(fetched) != 0 {
		t.Fatalf("row %v fetched %v", row, fetched)
	}
}

func TestNoProbeOpenServesCachedCatalogForBareCustomEndpoint(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	seedCustomModelCache(t, localCatalog())
	fetched := []string{}
	fetchAPIModelsFn = func(_ context.Context, _, baseURL string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
		fetched = append(fetched, baseURL)
		return nil, false
	}
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "custom", CurrentBaseURL: localEndpoint, CurrentModel: "omlx-model-1",
		UserProviders: map[string]any{}, CustomProviders: []any{}, ForPicker: true, SkipCustomProviderProbe: true})
	row := pickerRow(rows, "custom")
	if row == nil || !reflect.DeepEqual(rowModels(row), localCatalog()) || len(fetched) != 0 {
		t.Fatalf("row %v fetched %v", row, fetched)
	}
}

func TestNoProbeOpenWithoutCacheKeepsConfiguredModelsAndStaysOffline(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	seedCustomModelCache(t, []string{})
	row, fetched := noProbeRow(t, ctx, localEndpoint, AuthenticatedProvidersOptions{CustomProviders: []any{localEntry("omlx-model-1")}})
	if row == nil || !reflect.DeepEqual(rowModels(row), []string{"omlx-model-1"}) || len(fetched) != 0 {
		t.Fatalf("row %v fetched %v", row, fetched)
	}
}

func TestNoProbeOpenRespectsDiscoverModelsFalse(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	seedCustomModelCache(t, localCatalog())
	entry := localEntry("pinned-model")
	entry["models"], entry["discover_models"] = []any{"pinned-model"}, false
	row, fetched := noProbeRow(t, ctx, localEndpoint, AuthenticatedProvidersOptions{CustomProviders: []any{entry}})
	if row == nil || !reflect.DeepEqual(rowModels(row), []string{"pinned-model"}) || len(fetched) != 0 {
		t.Fatalf("row %v fetched %v", row, fetched)
	}
}

func TestCachedCatalogIsNotWrittenBackToConfig(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	seedCustomModelCache(t, localCatalog())
	saves := recordDiscoveredSaves(t)
	row, _ := noProbeRow(t, ctx, localEndpoint, AuthenticatedProvidersOptions{CustomProviders: []any{localEntry("omlx-model-1")}})
	if row == nil || !reflect.DeepEqual(rowModels(row), localCatalog()) || len(*saves) != 0 {
		t.Fatalf("row %v saves %+v", row, *saves)
	}
}

func TestKeylessEndpointWithSavedCatalogStillReadsCache(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	seedCustomModelCache(t, localCatalog())
	entry := localEntry("omlx-model-1")
	entry["models"] = []any{"omlx-model-1"}
	row, fetched := noProbeRow(t, ctx, localEndpoint, AuthenticatedProvidersOptions{CustomProviders: []any{entry}})
	if row == nil || !reflect.DeepEqual(rowModels(row), localCatalog()) || len(fetched) != 0 {
		t.Fatalf("row %v fetched %v", row, fetched)
	}
}

func TestKeylessEndpointWithSavedCatalogIsStillNotProbed(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	seedCustomModelCache(t, []string{})
	fetched := []string{}
	fetchAPIModelsFn = func(_ context.Context, _, baseURL string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
		fetched = append(fetched, baseURL)
		return []string{"should-not-be-reached"}, true
	}
	entry := localEntry("omlx-model-1")
	entry["models"] = []any{"omlx-model-1"}
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "nous", UserProviders: map[string]any{}, CustomProviders: []any{entry}, ForPicker: true})
	row := rowWithAPIURL(rows, localEndpoint)
	if len(row) == 0 || !reflect.DeepEqual(rowModels(row[0]), []string{"omlx-model-1"}) || len(fetched) != 0 {
		t.Fatalf("rows %v fetched %v", row, fetched)
	}
}

func TestApiModeRowsDoNotShareACachedCatalog(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	openaiCatalog := []string{"gpt-oss-a", "gpt-oss-b"}
	seedEndpointCache(t, sharedProxyURL, "sk-shared", openaiCatalog, 10*time.Second)
	anthropicRow, fetchedA := noProbeRow(t, ctx, sharedProxyURL, AuthenticatedProvidersOptions{CustomProviders: []any{map[string]any{
		"name": "Proxy Anthropic", "base_url": sharedProxyURL, "api_key": "sk-shared", "api_mode": "anthropic_messages", "model": "claude-via-proxy"}}})
	openaiRow, fetchedB := noProbeRow(t, ctx, sharedProxyURL, AuthenticatedProvidersOptions{CustomProviders: []any{map[string]any{
		"name": "Proxy OpenAI", "base_url": sharedProxyURL, "api_key": "sk-shared", "model": "gpt-via-proxy"}}})
	if anthropicRow == nil || openaiRow == nil {
		t.Fatalf("rows %v %v", anthropicRow, openaiRow)
	}
	if !reflect.DeepEqual(rowModels(anthropicRow), []string{"claude-via-proxy"}) {
		t.Fatalf("an anthropic_messages row must not render the OpenAI-mode catalog cached against the same base_url %v", anthropicRow)
	}
	if !reflect.DeepEqual(rowModels(openaiRow), openaiCatalog) || len(fetchedA)+len(fetchedB) != 0 {
		t.Fatalf("row %v fetched %v %v", openaiRow, fetchedA, fetchedB)
	}
}

func firstCustomProviderEntry(t *testing.T, h string) map[string]any {
	t.Helper()
	entries, _ := readConfigYAMLMap(t, h)["custom_providers"].([]any)
	if len(entries) == 0 {
		t.Fatalf("no custom_providers saved")
	}
	entry, _ := entries[0].(map[string]any)
	return entry
}

func noSentinelIDs(ids []string) bool {
	for _, m := range ids {
		if strings.HasPrefix(m, "__") {
			return false
		}
	}
	return true
}

func TestAutoSavedCatalogRoundTripsWithoutPinning(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	h := os.Getenv("ICEBATH_HOME")
	writeConfigYAML(t, h, "custom_providers:\n  - name: Local MLX\n    base_url: "+localEndpoint+"\n    model: omlx-model-1\n")
	saveDiscoveredModelsToConfig(ctx, localEndpoint, localCatalog(), "", nil, nil)
	saved := firstCustomProviderEntry(t, h)
	models := listedModelIDs(t, saved["models"])
	if saved["models_discovered"] != true || !reflect.DeepEqual(models, localCatalog()) {
		t.Fatalf("saved %v", saved)
	}
	if !noSentinelIDs(models) {
		t.Fatalf("sentinel keys must never appear inside the user-facing models mapping %v", models)
	}
	newer := append(localCatalog(), "omlx-model-9")
	seedCustomModelCache(t, newer)
	row, fetched := noProbeRow(t, ctx, localEndpoint, AuthenticatedProvidersOptions{CustomProviders: []any{saved}})
	if row == nil || !reflect.DeepEqual(rowModels(row), newer) {
		t.Fatalf("an auto-saved catalog must not pin the endpoint against a newer cached lineup %v", row)
	}
	if len(fetched) != 0 {
		t.Fatalf("fetched %v", fetched)
	}
}

func TestLegacySentinelCatalogStillResolvesAndMigrates(t *testing.T) {
	ctx, _ := pickerTestHome(t, nil)
	models := map[string]any{"__discovered_model_catalog__": true}
	yamlModels := "      __discovered_model_catalog__: true\n"
	for _, m := range localCatalog() {
		models[m] = map[string]any{}
		yamlModels += "      " + m + ": {}\n"
	}
	legacy := map[string]any{"name": "Local MLX", "base_url": localEndpoint, "model": "omlx-model-1", "models": models}
	if !entryModelsDiscovered(legacy) || modelsConfigIsAllowlist(legacy["models"], entryModelsDiscovered(legacy)) || !reflect.DeepEqual(declaredModelIDs(legacy["models"]), localCatalog()) {
		t.Fatalf("legacy entry read wrong %v", declaredModelIDs(legacy["models"]))
	}
	copied := map[string]any{}
	for k, v := range legacy {
		copied[k] = v
	}
	normalized := NormalizeCustomProviderEntry(copied, "")
	if normalized == nil || normalized["models_discovered"] != true {
		t.Fatalf("normalized %v", normalized)
	}
	if ids := listedModelIDs(t, normalized["models"]); !reflect.DeepEqual(ids, localCatalog()) || !noSentinelIDs(ids) {
		t.Fatalf("normalized models %v", ids)
	}
	seedCustomModelCache(t, []string{})
	row, fetched := noProbeRow(t, ctx, localEndpoint, AuthenticatedProvidersOptions{CustomProviders: []any{legacy}})
	if row == nil || !noSentinelIDs(rowModels(row)) || len(fetched) != 0 {
		t.Fatalf("row %v fetched %v", row, fetched)
	}
	h := os.Getenv("ICEBATH_HOME")
	writeConfigYAML(t, h, "custom_providers:\n  - name: Local MLX\n    base_url: "+localEndpoint+"\n    model: omlx-model-1\n    models:\n"+yamlModels)
	saveDiscoveredModelsToConfig(ctx, localEndpoint, localCatalog(), "", nil, nil)
	saved := firstCustomProviderEntry(t, h)
	ids := listedModelIDs(t, saved["models"])
	if saved["models_discovered"] != true || !reflect.DeepEqual(ids, localCatalog()) || !noSentinelIDs(ids) {
		t.Fatalf("saved %v", saved)
	}
}
