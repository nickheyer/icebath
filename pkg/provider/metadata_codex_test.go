package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func codexCatalog(items ...map[string]any) map[string]any {
	list := make([]any, 0, len(items))
	for _, it := range items {
		list = append(list, it)
	}
	return map[string]any{"models": list}
}

func TestLiveCatalogueCacheIsScopedToAccessToken(t *testing.T) {
	ctx, ft, dir := metaTest(t)
	responses := []map[string]any{codexCatalog(map[string]any{"slug": "gpt-5.5", "context_window": 272_000}), codexCatalog(map[string]any{"slug": "gpt-5.5", "context_window": 372_000})}
	ft.handle = func(r *http.Request) (*http.Response, error) {
		next := responses[0]
		responses = responses[1:]
		return jsonResponse(nil, 200, next, nil), nil
	}
	reqFor := func(subject string) ContextLengthRequest {
		return ContextLengthRequest{Model: "gpt-5.5", BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: codexJWT(subject), Provider: "openai-codex"}
	}
	first := GetModelContextLength(ctx, reqFor("token-account-a"))
	firstAgain := GetModelContextLength(ctx, reqFor("token-account-a"))
	second := GetModelContextLength(ctx, reqFor("token-account-b"))
	if first != 272_000 || firstAgain != 272_000 || second != 372_000 {
		t.Fatalf("got %d %d %d", first, firstAgain, second)
	}
	mustEqual(t, ft.count(), 2, "probes")
	mustEqual(t, ft.calls[0].Headers.Get("Authorization"), "Bearer "+codexJWT("token-account-a"), "first auth")
	mustEqual(t, ft.calls[1].Headers.Get("Authorization"), "Bearer "+codexJWT("token-account-b"), "second auth")
	memoLock.Lock()
	for key := range codexOAuthContextCache {
		if strings.Contains(key, "token-account") {
			t.Fatal("cache keys must not carry the token")
		}
	}
	memoLock.Unlock()
	mustEqual(t, intOrDefault(readContextCacheFile(t, dir)["gpt-5.5@https://chatgpt.com/backend-api/codex"], 0), 372_000, "live value persisted")
}

func TestProbeFailureFallsBackToHardcoded(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 401, map[string]any{}, nil), nil
	}
	mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.5")), 272_000, "fallback")
}

func TestGatewayKeyNeverProbesTheDirectCatalog(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	live, fresh := fetchCodexOAuthContextLengthsWithSource(ctx, "gateway-pool-key", "")
	if len(live) != 0 || fresh {
		t.Fatal("gateway key must decline")
	}
	mustEqual(t, ft.count(), 0, "no probe")
}

func TestCustomBaseProbesItsOwnCatalog(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, codexCatalog(map[string]any{"slug": "gpt-5.5", "context_window": 272_000}), nil), nil
	}
	got := GetModelContextLength(ctx, ContextLengthRequest{Model: "gpt-5.5", BaseURL: "https://codex-gw.example/backend-api/codex", APIKey: codexJWT("acct"), Provider: "openai-codex"})
	mustEqual(t, got, 272_000, "ctx")
	if !strings.HasPrefix(ft.calls[0].URL, "https://codex-gw.example/backend-api/codex/models?client_version=") {
		t.Fatalf("probed %s", ft.calls[0].URL)
	}
}

func TestLiveCodexContextReplacesStaleCacheInBothDirections(t *testing.T) {
	for _, tc := range [][2]int{{272_000, 372_000}, {372_000, 272_000}} {
		ctx, ft, dir := metaTest(t)
		base := "https://chatgpt.com/backend-api/codex"
		writeContextCacheFile(t, dir, map[string]any{"gpt-5.5@" + base: tc[0], "other-model@https://api.openai.com/v1/": 128_000})
		ft.handle = func(r *http.Request) (*http.Response, error) {
			return jsonResponse(nil, 200, codexCatalog(map[string]any{"slug": "gpt-5.5", "context_window": tc[1]}), nil), nil
		}
		mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.5")), tc[1], "live")
		mustEqual(t, ft.count(), 1, "one probe")
		remaining := readContextCacheFile(t, dir)
		mustEqual(t, intOrDefault(remaining["gpt-5.5@"+base], 0), tc[1], "replaced")
		mustEqual(t, intOrDefault(remaining["other-model@https://api.openai.com/v1/"], 0), 128_000, "other kept")
	}
}

func TestBaseSlugKeepsAdvertised272k(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, codexCatalog(map[string]any{"slug": "gpt-5.6-sol", "context_window": 272_000}), nil), nil
	}
	mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol")), 272_000, "base")
}

func TestNon272kAdvertisementIsTrustedVerbatim(t *testing.T) {
	for _, advertised := range []int{372_000, 200_000, 1_050_000} {
		ctx, ft, _ := metaTest(t)
		ft.handle = func(r *http.Request) (*http.Response, error) {
			return jsonResponse(nil, 200, codexCatalog(map[string]any{"slug": "gpt-5.6-sol", "context_window": advertised}), nil), nil
		}
		mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol-900k")), advertised, "verbatim")
	}
}

func TestOptedInVariantCappedAtLiveCatalogMax(t *testing.T) {
	for _, base := range []string{"gpt-5.6-luna", "gpt-6.1-sol"} {
		for _, tc := range []struct{ max, want int }{{872_000, 872_000}, {0, 900_000}, {1_050_000, 900_000}} {
			ctx, ft, _ := metaTest(t)
			item := map[string]any{"slug": base, "context_window": 272_000}
			if tc.max > 0 {
				item["max_context_window"] = tc.max
			}
			ft.handle = func(r *http.Request) (*http.Response, error) {
				return jsonResponse(nil, 200, codexCatalog(item), nil), nil
			}
			mustEqual(t, GetModelContextLength(ctx, codexContextRequest(base+"-900k")), tc.want, base)
		}
	}
}

func TestBaseSlugKeepsAdvertisedCtxEvenWithCatalogMax(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, codexCatalog(map[string]any{"slug": "gpt-5.6-sol", "context_window": 272_000, "max_context_window": 872_000}), nil), nil
	}
	mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol")), 272_000, "base")
}

func TestFailedRefreshPastTTLKeepsTheObservedCatalogCap(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, codexCatalog(map[string]any{"slug": "GPT-5.6-Luna", "context_window": 272_000, "max_context_window": 600_000}), nil), nil
	}
	mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.6-luna-900k")), 600_000, "first")
	ft.handle = func(r *http.Request) (*http.Response, error) { return nil, errors.New("catalog down") }
	wallNow = func() time.Time { return time.Now().Add(codexOAuthContextCacheTTL + time.Second) }
	mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.6-luna-900k")), 600_000, "after failed refresh")
}

func TestAProbeThatFindsNoCatalogIsNotRepeatedWithinTheNegativeTTL(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) { return nil, errors.New("catalog down") }
	mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol")), 272_000, "down")
	perProbe := ft.count()
	GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol"))
	GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol-900k"))
	if perProbe == 0 || ft.count() != perProbe {
		t.Fatalf("re-probed inside the negative ttl: %d then %d", perProbe, ft.count())
	}
	wallNow = func() time.Time { return time.Now().Add(codexOAuthContextNegativeTTL + time.Second) }
	GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol"))
	mustEqual(t, ft.count(), 2*perProbe, "retried after the negative ttl")
}

func TestCatalogMaxIsPublishedBeforeTheContextEntry(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, codexCatalog(map[string]any{"slug": "gpt-5.6-luna", "context_window": 272_000, "max_context_window": 872_000}), nil), nil
	}
	live, fresh := fetchCodexOAuthContextLengthsWithSource(ctx, codexJWT("fake-token"), "")
	if !fresh || live["gpt-5.6-luna"] != 272_000 {
		t.Fatalf("live %v fresh %v", live, fresh)
	}
	key := codexOAuthTokenFingerprint(codexJWT("fake-token"), "")
	memoLock.Lock()
	defer memoLock.Unlock()
	if codexOAuthMaxContextCache[key]["gpt-5.6-luna"] != 872_000 {
		t.Fatal("the cap must be published with the context entry")
	}
}

func TestFallbackTableResolutionAlsoBumped(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		return jsonResponse(nil, 401, map[string]any{}, nil), nil
	}
	mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol-900k")), 900_000, "bumped")
	mustEqual(t, GetModelContextLength(ctx, codexContextRequest("gpt-5.6-sol")), 272_000, "base stays")
}

func Test900kEligibilityTable(t *testing.T) {
	table := []struct {
		model string
		valid bool
		ctx   int
		wire  string
	}{
		{"gpt-5.6-sol-900k", true, 900_000, "gpt-5.6-sol"},
		{"gpt-5.6-sol-2026-07-09-900k", true, 900_000, "gpt-5.6-sol-2026-07-09"},
		{"openai/gpt-5.6-sol-900k", true, 900_000, "openai/gpt-5.6-sol"},
		{"gpt-5.6-sol-pro-900k", false, 272_000, "gpt-5.6-sol-pro-900k"},
		{"gpt-5.5-900k", false, 272_000, "gpt-5.5-900k"},
		{"gpt-5.6-nova-900k", false, 272_000, "gpt-5.6-nova-900k"},
	}
	for _, row := range table {
		mustEqual(t, IsCodexContextVariant(row.model), row.valid, row.model+" valid")
		mustEqual(t, StripCodexContextVariantSuffix(row.model), row.wire, row.model+" wire")
		ctx, ft, _ := metaTest(t)
		slug := strings.TrimSuffix(StripCodexContextVariantSuffix(lastPathSegment(row.model)), "-900k")
		ft.handle = func(r *http.Request) (*http.Response, error) {
			return jsonResponse(nil, 200, codexCatalog(map[string]any{"slug": slug, "context_window": 272_000}), nil), nil
		}
		mustEqual(t, GetModelContextLength(ctx, codexContextRequest(row.model)), row.ctx, row.model+" ctx")
	}
}

func TestFetchEndpointModelMetadataAuthFailureStopsAfterFirstCandidate(t *testing.T) {
	for _, status := range []int{401, 403} {
		ctx, ft, _ := metaTest(t)
		ft.handle = func(r *http.Request) (*http.Response, error) {
			return jsonResponse(nil, status, map[string]any{}, nil), nil
		}
		if got := FetchEndpointModelMetadata(ctx, "https://custom.example/v1", "", false); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
		mustEqual(t, ft.count(), 1, "one candidate")
		if got := FetchEndpointModelMetadata(ctx, "https://custom.example/v1", "", false); len(got) != 0 {
			t.Fatalf("second got %v", got)
		}
		mustEqual(t, ft.count(), 1, "empty result cached")
	}
}

func TestFetchEndpointModelMetadataNotFoundStillTriesAlternateCandidate(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", "https://custom.example/v1/models", 404, map[string]any{})
	ft.on("GET", "https://custom.example/models", 200, map[string]any{"data": []any{map[string]any{"id": "test/model", "context_length": 32768}}})
	got := FetchEndpointModelMetadata(ctx, "https://custom.example/v1", "", false)
	mustEqual(t, intOrDefault(got["test/model"]["context_length"], 0), 32768, "alternate")
	urls := ft.urls()
	if len(urls) != 2 || urls[0] != "https://custom.example/v1/models" || urls[1] != "https://custom.example/models" {
		t.Fatalf("urls %v", urls)
	}
}

func TestRemoteProbeIsMemoizedOnDiskAcrossProcesses(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.on("GET", "https://custom.example/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "test/model", "context_length": 32768}}})
	mustEqual(t, intOrDefault(FetchEndpointModelMetadata(ctx, "https://custom.example/v1", "", false)["test/model"]["context_length"], 0), 32768, "live")
	memoLock.Lock()
	endpointModelMetadataCache = map[endpointMemoKey]map[string]map[string]any{}
	endpointModelMetadataCacheTime = map[endpointMemoKey]time.Time{}
	memoLock.Unlock()
	mustEqual(t, intOrDefault(FetchEndpointModelMetadata(ctx, "https://custom.example/v1", "", false)["test/model"]["context_length"], 0), 32768, "disk memo")
	mustEqual(t, ft.count(), 1, "no re-probe")
	memoLock.Lock()
	endpointModelMetadataCache = map[endpointMemoKey]map[string]map[string]any{}
	endpointModelMetadataCacheTime = map[endpointMemoKey]time.Time{}
	memoLock.Unlock()
	wallNow = func() time.Time { return time.Now().Add(endpointModelCacheTTL + time.Second) }
	FetchEndpointModelMetadata(ctx, "https://custom.example/v1", "", false)
	mustEqual(t, ft.count(), 2, "expired memo re-probes")
}

func TestEmptyModelNeverFuzzyMatchesEndpointCatalog(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	nous := "https://inference-api.nousresearch.com/v1"
	ft.on("GET", nous+"/models", 200, map[string]any{"data": []any{map[string]any{"id": "voyageai/voyage-code-4", "context_length": 32_000}, map[string]any{"id": "x-ai/grok-4.6", "context_length": 500_000}}})
	if _, ok := resolveEndpointContextLength(ctx, "", nous, ""); ok {
		t.Fatal("empty model must not fuzzy match")
	}
	if got, ok := resolveEndpointContextLength(ctx, "grok-4.6", nous, ""); !ok || got != 500_000 {
		t.Fatal("non empty names fuzzy match")
	}
	seedServerType("http://localhost:8080/v1", "")
	ft.on("GET", "http://127.0.0.1:8080/v1/models", 200, map[string]any{"data": []any{map[string]any{"id": "only-model", "context_length": 131_072}}})
	if got, ok := resolveEndpointContextLength(ctx, "", "http://localhost:8080/v1", ""); !ok || got != 131_072 {
		t.Fatalf("single model endpoints resolve with an empty name, got %d %v", got, ok)
	}
}

func TestOpenRouterFallbackIsNotPersisted(t *testing.T) {
	ctx, ft, dir := metaTest(t)
	nous := "https://inference-api.nousresearch.com/v1"
	ft.on("GET", nous+"/models", 200, map[string]any{"data": []any{}})
	ft.on("GET", "https://inference-api.nousresearch.com/models", 200, map[string]any{"data": []any{}})
	seedOpenRouterCatalog(map[string]map[string]any{"qwen/qwen3.6-plus": {"context_length": 1_000_000}})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "qwen3.6-plus", BaseURL: nous, APIKey: "fake", Provider: "nous"}), 1_000_000, "or fallback")
	if len(readContextCacheFile(t, dir)) != 0 {
		t.Fatal("OR fallback must not be persisted")
	}
}

func TestStaleCacheIsBypassedAndOverwrittenByPortal(t *testing.T) {
	ctx, ft, dir := metaTest(t)
	nous := "https://inference-api.nousresearch.com/v1"
	writeContextCacheFile(t, dir, map[string]any{"qwen3.6-plus@" + nous: 1_000_000, "other-model@https://api.openai.com/v1": 128_000})
	ft.on("GET", nous+"/models", 200, map[string]any{"data": []any{map[string]any{"id": "qwen3.6-plus", "context_length": 262_144}}})
	seedOpenRouterCatalog(map[string]map[string]any{})
	mustEqual(t, GetModelContextLength(ctx, ContextLengthRequest{Model: "qwen3.6-plus", BaseURL: nous, APIKey: "fake", Provider: "nous"}), 262_144, "portal")
	remaining := readContextCacheFile(t, dir)
	mustEqual(t, intOrDefault(remaining["qwen3.6-plus@"+nous], 0), 262_144, "overwritten")
	mustEqual(t, intOrDefault(remaining["other-model@https://api.openai.com/v1"], 0), 128_000, "untouched")
}
