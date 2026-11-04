package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestParseOpenRouterReasoningCapabilities(t *testing.T) {
	caps, ok := ParseOpenRouterReasoningCapabilities(map[string]any{
		"id": "nvidia/nemotron-3-ultra", "supported_parameters": []any{"temperature", "tools", "reasoning"},
		"reasoning": map[string]any{"mandatory": false, "supported_efforts": []any{"low", "medium", "high"}},
	})
	if !ok || !caps.SupportsReasoning || caps.Mandatory || !modelsEqual(caps.SupportedEfforts, []string{"low", "medium", "high"}) {
		t.Fatalf("supported with efforts: %+v %v", caps, ok)
	}
	caps, ok = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"reasoning", "tools"}, "reasoning": map[string]any{}})
	if !ok || !caps.SupportsReasoning || caps.SupportedEfforts != nil || caps.Mandatory {
		t.Fatalf("every effort accepted when the field is omitted: %+v", caps)
	}
	caps, ok = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"reasoning"}})
	if !ok || !caps.SupportsReasoning || caps.SupportedEfforts != nil {
		t.Fatalf("supported_parameters alone is authoritative: %+v", caps)
	}
	caps, _ = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"reasoning"}, "reasoning": map[string]any{"mandatory": true, "supported_efforts": nil}})
	if !caps.Mandatory {
		t.Fatal("mandatory flag")
	}
	caps, ok = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"temperature", "tools"}, "reasoning": map[string]any{"supported_efforts": []any{"high"}}})
	if !ok || caps.SupportsReasoning || caps.SupportedEfforts != nil {
		t.Fatalf("reasoning object untrusted without the parameter: %+v", caps)
	}
	for _, item := range []any{map[string]any{"id": "x"}, map[string]any{"supported_parameters": "bad"}, "not-a-dict"} {
		if _, ok := ParseOpenRouterReasoningCapabilities(item); ok {
			t.Fatalf("%v is unknown", item)
		}
	}
	caps, _ = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"reasoning"}, "reasoning": map[string]any{"supported_efforts": []any{" High ", "high", "LOW", "", 3}}})
	if !modelsEqual(caps.SupportedEfforts, []string{"high", "low", "3"}) {
		t.Fatalf("efforts normalized and deduped: %v", caps.SupportedEfforts)
	}
}

var reasoningCapsTestCatalog = map[string]any{"data": []any{
	map[string]any{"id": "deepseek/deepseek-v4-pro", "supported_parameters": []any{"reasoning", "tools"}, "reasoning": map[string]any{"mandatory": false, "supported_efforts": []any{"xhigh", "high"}}},
	map[string]any{"id": "arcee-ai/trinity-large-thinking", "supported_parameters": []any{"reasoning"}, "reasoning": map[string]any{"mandatory": true}},
}}

// Cold process with no mirror on disk and background warms run inline
func reasoningCapsColdProcess(t *testing.T) context.Context {
	t.Helper()
	ctx := modelsTestHome(t)
	ResetReasoningCapsCatalogs()
	os.Remove(reasoningCapsDiskPath())
	nousInferenceEnvOverrideFn = func() string { return "" }
	resolveNousRuntimeCredentialsBackgroundFn = func(context.Context) (string, string, bool) { return "", "", false }
	reasoningCapsSpawn = func(refresh func()) { refresh() }
	t.Cleanup(func() {
		ResetReasoningCapsCatalogs()
		nousInferenceEnvOverrideFn = nousInferenceEnvOverride
		resolveNousRuntimeCredentialsBackgroundFn = resolveNousRuntimeCredentialsBackground
		reasoningCapsSpawn = func(refresh func()) { go refresh() }
	})
	return ctx
}

func reasoningCapsServe(payload map[string]any, seen *[]*http.Request) {
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		if seen != nil {
			*seen = append(*seen, req)
		}
		return modelsJSONResponse(req, 200, payload), nil
	})
}

func reasoningCapsOffline(t *testing.T) {
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("must not fetch: %s", req.URL)
		return nil, nil
	})
}

func TestOpenRouterModelReasoningCapabilities(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	reasoningCapsServe(map[string]any{"data": []any{map[string]any{"id": "a/b", "supported_parameters": []any{"reasoning"}}}}, nil)
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "a/b", 0, true); !ok {
		t.Fatal("fetched catalog answers")
	}
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "private/custom", 0, false); ok {
		t.Fatal("unlisted model is unknown")
	}
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "", 0, false); ok {
		t.Fatal("empty model is unknown")
	}
	ctx = reasoningCapsColdProcess(t)
	calls := 0
	modelsTransport = modelsFakeTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("offline")
	})
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "a/b", 0, true); ok {
		t.Fatal("unreachable is unknown")
	}
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "a/b", 0, true); ok || calls != 1 {
		t.Fatalf("second call inside the failure TTL must not refetch: calls=%d", calls)
	}
	ctx = reasoningCapsColdProcess(t)
	reasoningCapsOffline(t)
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "a/b", 0, false); ok {
		t.Fatal("cache only by default never fetches")
	}
}

func TestNousModelReasoningCapabilities(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	var seen []*http.Request
	reasoningCapsServe(reasoningCapsTestCatalog, &seen)
	optional, ok := NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, true)
	if !ok || !optional.SupportsReasoning || optional.Mandatory || !modelsEqual(optional.SupportedEfforts, []string{"xhigh", "high"}) {
		t.Fatalf("fetch parses the mandatory flag: %+v", optional)
	}
	mandatory, ok := NousModelReasoningCapabilities(ctx, "arcee-ai/trinity-large-thinking", 0, false)
	if !ok || !mandatory.Mandatory {
		t.Fatalf("mandatory route: %+v", mandatory)
	}
	if seen[0].Header.Get("User-Agent") == "" {
		t.Fatal("the Portal 403s an anonymous catalog read")
	}
	if seen[0].URL.String() != defaultNousInferenceURL+"/v1/models" {
		t.Fatalf("production catalog url: %s", seen[0].URL)
	}
	if _, ok := NousModelReasoningCapabilities(ctx, "private/route", 0, true); ok {
		t.Fatal("unlisted is unknown")
	}
	if _, ok := NousModelReasoningCapabilities(ctx, "", 0, false); ok {
		t.Fatal("empty is unknown")
	}
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false); ok {
		t.Fatal("a Portal fetch must not answer for OpenRouter")
	}

	ctx = reasoningCapsColdProcess(t)
	reasoningCapsOffline(t)
	if _, ok := NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false); ok {
		t.Fatal("cache only by default never fetches")
	}
	ctx = reasoningCapsColdProcess(t)
	calls := 0
	modelsTransport = modelsFakeTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("offline")
	})
	NousModelReasoningCapabilities(ctx, "a/b", 0, true)
	if _, ok := NousModelReasoningCapabilities(ctx, "a/b", 0, true); ok || calls != 1 {
		t.Fatalf("unreachable catalog rate limits the refetch: calls=%d", calls)
	}
}

func TestNousCatalogURLFollowsTheConfiguredEndpoint(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	nousInferenceEnvOverrideFn = func() string { return "https://staging.nousresearch.com/v1" }
	if got := NousCatalogURL(ctx); got != "https://staging.nousresearch.com/v1/models" {
		t.Fatalf("staging url: %q", got)
	}
	nousInferenceEnvOverrideFn = func() string { return "" }
	if got := NousCatalogURL(ctx); got != defaultNousInferenceURL+"/v1/models" {
		t.Fatalf("production url: %q", got)
	}
}

func TestFetchedCatalogAnswersALaterProcessOffline(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	reasoningCapsServe(reasoningCapsTestCatalog, nil)
	if _, ok := NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, true); !ok {
		t.Fatal("online run")
	}
	ResetReasoningCapsCatalogs()
	reasoningCapsOffline(t)
	caps, ok := NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false)
	if !ok || caps.Mandatory {
		t.Fatalf("the mirror answers the next process: %+v %v", caps, ok)
	}
	if caps, ok := NousModelReasoningCapabilities(ctx, "arcee-ai/trinity-large-thinking", 0, false); !ok || !caps.Mandatory {
		t.Fatalf("mandatory verdict survives: %+v", caps)
	}
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false); ok {
		t.Fatal("the mirror is keyed by catalog url")
	}
	ResetReasoningCapsCatalogs()
	nousInferenceEnvOverrideFn = func() string { return "https://staging.nousresearch.com" }
	if _, ok := NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false); ok {
		t.Fatal("a staging Portal does not read production's mirror")
	}
}

func TestStaleCopyIsStillServedAndRefreshed(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	rawURL := NousCatalogURL(ctx)
	catalogFor(rawURL).saveDisk(map[string]reasoningCaps{"deepseek/deepseek-v4-pro": {known: true, supports: true}})
	path := reasoningCapsDiskPath()
	raw, _ := os.ReadFile(path)
	doc := map[string]any{}
	json.Unmarshal(raw, &doc)
	entry, _ := asMap(doc[rawURL])
	entry["ts"] = 0
	raw, _ = json.Marshal(doc)
	os.WriteFile(path, raw, 0o600)
	ResetReasoningCapsCatalogs()
	refreshed := 0
	reasoningCapsSpawn = func(refresh func()) { refreshed++ }
	reasoningCapsOffline(t)
	caps, ok := NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false)
	if !ok || caps.Mandatory || !caps.SupportsReasoning || refreshed != 1 {
		t.Fatalf("a stale verdict beats no verdict and queues a refresh: %+v %v refreshed=%d", caps, ok, refreshed)
	}
}

func TestUnreadableMirrorDegradesToUnknown(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	path := reasoningCapsDiskPath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("{ this is not json"), 0o600)
	reasoningCapsOffline(t)
	if _, ok := NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false); ok {
		t.Fatal("a corrupt file answers unknown")
	}
}

func TestPricingFetchSeedsTheMirror(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	ResetPricingCache()
	pricingGetJSON = func(string, map[string]string, time.Duration) map[string]any { return reasoningCapsTestCatalog }
	t.Cleanup(func() { pricingGetJSON = pricingGetJSONHTTP })
	FetchModelsWithPricing(PricingFetchOptions{BaseURL: defaultNousInferenceURL, ForceRefresh: true})
	ResetReasoningCapsCatalogs()
	reasoningCapsOffline(t)
	if _, ok := NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false); !ok {
		t.Fatal("the pricing fetch mirrors the catalog for free")
	}
}

func TestAMissingMirrorIsLookedForOncePerProcess(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	reasoningCapsOffline(t)
	urls := 0
	t.Cleanup(func() { nousCatalogURLFn = NousCatalogURL })
	nousCatalogURLFn = func(ctx context.Context) string {
		urls++
		return NousCatalogURL(ctx)
	}
	for i := 0; i < 5; i++ {
		NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false)
	}
	if urls != 1 {
		t.Fatalf("naming the Portal catalog is paid once: %d", urls)
	}
	other := home.WithOverride(context.Background(), t.TempDir())
	NousModelReasoningCapabilities(other, "deepseek/deepseek-v4-pro", 0, false)
	if urls != 2 {
		t.Fatalf("a routed profile resolves its own catalog once: %d", urls)
	}
}

func TestWarmAndRefreshAsync(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	fetched := 0
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		fetched++
		return modelsJSONResponse(req, 200, reasoningCapsTestCatalog), nil
	})
	WarmNousReasoningCapsAsync(ctx)
	WarmNousReasoningCapsAsync(ctx)
	if fetched != 1 {
		t.Fatalf("one warm per process: %d", fetched)
	}
	if _, ok := NousModelReasoningCapabilities(ctx, "deepseek/deepseek-v4-pro", 0, false); !ok {
		t.Fatal("warm filled the cache")
	}
	RefreshReasoningCapsAsync(ctx, "nous-portal")
	RefreshReasoningCapsAsync(ctx, "unknown")
	if fetched != 2 {
		t.Fatalf("a forced refresh refetches: %d", fetched)
	}
	WarmOpenRouterReasoningCapsAsync(ctx)
	if fetched != 3 {
		t.Fatalf("the OpenRouter warm is its own fetch: %d", fetched)
	}
}
