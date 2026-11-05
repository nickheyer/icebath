package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func TestDetectShortAliasResolvesToStaticModel(t *testing.T) {
	ctx := modelsTestHome(t)
	fetchLiveCatalogIndexFn = func(context.Context, string, time.Duration) ([]any, map[string]map[string]any, bool) {
		t.Fatal("network lookup should not run")
		return nil, nil, false
	}
	providerHasCredentialsFn = func(context.Context, string) bool { return true }
	provider, model, ok := DetectProviderForModel(ctx, "sonnet", "auto")
	if !ok || provider != "anthropic" || !strings.HasPrefix(model, "claude-sonnet") {
		t.Fatalf("got %q %q %v", provider, model, ok)
	}
}

func TestDetectCustomProviderNotOverriddenByStaticCatalog(t *testing.T) {
	ctx := modelsTestHome(t)
	findOpenRouterSlugFn = func(context.Context, string) string { return "openai/gpt-5.4" }
	providerHasCredentialsFn = func(context.Context, string) bool { return false }
	if provider, model, ok := DetectProviderForModel(ctx, "gpt-5.4", "custom:foo"); ok {
		t.Fatalf("custom endpoints are never auto switched away from: %q %q", provider, model)
	}
}

func TestDetectStaticBareProviderNameSwitchesToCostSafeDefault(t *testing.T) {
	ctx := modelsTestHome(t)
	provider, model, ok := DetectStaticProviderForModel(ctx, "nous", "openrouter")
	if !ok || provider != "nous" || model != GetDefaultModelForProvider(ctx, "nous") || model == "" {
		t.Fatalf("got %q %q %v", provider, model, ok)
	}
	if _, _, ok := DetectStaticProviderForModel(ctx, "custom", "openrouter"); ok {
		t.Fatal("custom has no catalog to switch to")
	}
	if _, _, ok := DetectStaticProviderForModel(ctx, "openrouter", "nous"); ok {
		t.Fatal("openrouter needs an explicit model")
	}
	curated := staticCatalogModels("deepseek")
	if _, _, ok := DetectStaticProviderForModel(ctx, curated[0], "deepseek"); ok {
		t.Fatal("a model in the current provider's own catalog never suggests switching")
	}
	provider, model, ok = DetectStaticProviderForModel(ctx, strings.ToUpper(curated[0]), "openrouter")
	if !ok || provider != "deepseek" || model != strings.ToUpper(curated[0]) {
		t.Fatalf("case insensitive static match: %q %q %v", provider, model, ok)
	}
}

func TestParseModelInput(t *testing.T) {
	ctx := modelsTestHome(t)
	custom := map[string]bool{"custom": true, "custom:local": true, "custom:my-proxy": true}
	cases := []struct {
		raw, current, provider, model string
	}{
		{"nous:hermes-4", "openrouter", "nous", "hermes-4"},
		{"anthropic/claude-3.5-sonnet:beta", "openrouter", "openrouter", "anthropic/claude-3.5-sonnet:beta"},
		{"custom:local:qwen", "nous", "custom:local", "qwen"},
		{"custom:qwen", "nous", "custom", "qwen"},
		{"custom:other:model", "nous", "custom", "other:model"},
		{"custom:my-proxy:gpt", "nous", "custom:my-proxy", "gpt"},
		{"  gpt-5.4  ", "openai", "openai", "gpt-5.4"},
		{"claude:claude-opus-4", "nous", "anthropic", "claude-opus-4"},
		{"unknownthing:model", "nous", "nous", "unknownthing:model"},
	}
	for _, c := range cases {
		provider, model := ParseModelInput(ctx, c.raw, c.current, custom)
		if provider != c.provider || model != c.model {
			t.Fatalf("%q: got %q %q want %q %q", c.raw, provider, model, c.provider, c.model)
		}
	}
}

func TestProviderLabel(t *testing.T) {
	if ProviderLabel("auto") != "Auto" || ProviderLabel("") != CatalogProviderLabel("openrouter") {
		t.Fatal("auto and empty labels")
	}
	if ProviderLabel("claude") != CatalogProviderLabel("anthropic") {
		t.Fatal("aliases resolve to the canonical label")
	}
	if ProviderLabel("Weird-Thing") != "Weird-Thing" {
		t.Fatal("unknown ids echo the original spelling")
	}
}

func TestEachHomeSeesOnlyItsOwnStaticModels(t *testing.T) {
	modelsTestHome(t)
	root := t.TempDir()
	const slug = "acme-static"
	homes := map[string]string{"f": root + "/f", "g": root + "/g", "a": root + "/a"}
	snapshotProviderRegistry(t)
	for _, k := range []string{"f", "g"} {
		writeProviderPluginDir(t, homes[k], slug)
	}
	fakeExternalProviderPlugins(t, map[string]func(string) ([]Profile, error){slug: func(homeKey string) ([]Profile, error) {
		models := []string{"acme-f-only"}
		if homeKey == home.Key(homes["g"]) {
			models = []string{"acme-g-small", "acme-g-large"}
		}
		return []Profile{{Name: slug, DisplayName: "Acme Static", AuthType: AuthTypeAPIKey, KeyEnv: []string{"ACME_STATIC_KEY"},
			BaseURL: "https://acme-static.invalid/v1", StaticModels: models}}, nil
	}})
	view := func(h string) (string, []string, bool) {
		ctx := home.WithOverride(context.Background(), h)
		SyncPluginProviderCatalog()
		return GetDefaultModelForProvider(ctx, slug), StaticCatalog(ctx, slug), containsString(StaticCatalogProviders(ctx), slug)
	}
	check := func(h, def string, models []string, listed bool) {
		d, m, l := view(h)
		if d != def || !modelsEqual(m, models) || l != listed {
			t.Fatalf("%s: got %q %v %v", h, d, m, l)
		}
	}
	check(homes["f"], "acme-f-only", []string{"acme-f-only"}, true)
	check(homes["g"], "acme-g-small", []string{"acme-g-small", "acme-g-large"}, true)
	check(homes["a"], "", []string{}, false)
	check(homes["f"], "acme-f-only", []string{"acme-f-only"}, true)
}

func TestStaticCatalogProvidersKeepLadderOrder(t *testing.T) {
	ctx := modelsTestHome(t)
	order := StaticCatalogProviders(ctx)
	idx := func(p string) int {
		for i, x := range order {
			if x == p {
				return i
			}
		}
		return -1
	}
	if idx("nous") < 0 || idx("anthropic") < 0 || idx("nous") > idx("anthropic") || idx("openai") > idx("anthropic") {
		t.Fatalf("hermes table order expected: %v", order)
	}
}

func TestPickSilentDefaultModel(t *testing.T) {
	ctx := modelsTestHome(t)
	if PickSilentDefaultModel(ctx, nil, "openrouter") != "" {
		t.Fatal("empty list")
	}
	ids := []string{"anthropic/claude-fable-5", "anthropic/claude-opus-5", "qwen/qwen3.6-plus"}
	if got := PickSilentDefaultModel(ctx, ids, "openrouter"); got != "qwen/qwen3.6-plus" {
		t.Fatalf("first non frontier row expected, got %q", got)
	}
	if got := PickSilentDefaultModel(ctx, append([]string{PreferredSilentDefaultModel}, ids...), "openrouter"); got != PreferredSilentDefaultModel {
		t.Fatalf("labeled default wins, got %q", got)
	}
	if got := PickSilentDefaultModel(ctx, ids[:2], "openrouter"); got != ids[0] {
		t.Fatalf("all frontier falls back to the first row, got %q", got)
	}
	if !isAnthropicFrontierTier("anthropic/claude-opus-4-6") || isAnthropicFrontierTier("qwopus3.6-27b-coder") || isAnthropicFrontierTier("anthropic/claude-sonnet-4-6") {
		t.Fatal("frontier tier predicate")
	}
}

func TestMergeWithModelsDev(t *testing.T) {
	ctx := modelsTestHome(t)
	listAgenticModelsFn = func(context.Context, string, bool) []string { return nil }
	if got := mergeWithModelsDev(ctx, "opencode-go", []string{"mimo-v2-pro", "kimi-k2.6"}); !modelsEqual(got, []string{"mimo-v2-pro", "kimi-k2.6"}) {
		t.Fatalf("empty models.dev keeps curated: %v", got)
	}
	listAgenticModelsFn = func(context.Context, string, bool) []string { return []string{"MiniMax-M2.7"} }
	if got := mergeWithModelsDev(ctx, "minimax", []string{"minimax-m2.7", "minimax-m2.5"}); !modelsEqual(got, []string{"MiniMax-M2.7", "minimax-m2.5"}) {
		t.Fatalf("case insensitive dedup keeps models.dev casing: %v", got)
	}
}

func TestDeepseekPickerIgnoresModelsDevRetiredIDs(t *testing.T) {
	ctx := modelsTestHome(t)
	listAgenticModelsFn = func(context.Context, string, bool) []string {
		return []string{"deepseek-v4-flash-vision-exp", "deepseek-v4-flash", "deepseek-flash", "deepseek-v4-pro"}
	}
	saved := providerCatalogFetchers
	providerCatalogFetchers = map[string]catalogFetcher{}
	t.Cleanup(func() { providerCatalogFetchers = saved })
	profileLiveCatalogFn = func(context.Context, string) (CatalogRows, bool) { return CatalogRows{}, false }
	out := ProviderModelIDs(ctx, "deepseek", false)
	if !modelsEqual(out.IDs, staticCatalogModels("deepseek")) || containsString(out.IDs, "deepseek-v4-flash") {
		t.Fatalf("curated only deepseek: %v", out.IDs)
	}
}

func TestLiveBareK3FoldsIntoCuratedSlug(t *testing.T) {
	ctx := modelsTestHome(t)
	apiKeyCredentialsFn = func(context.Context, string) (string, string) { return "sk-kimi-x", "https://api.kimi.com/coding" }
	profileFetchModelsFn = func(context.Context, Profile, string, string, time.Duration) ([]string, bool) {
		return []string{"k3", "kimi-for-coding"}, true
	}
	out := ProviderModelIDs(ctx, "kimi-coding", false).IDs
	var k3 []string
	for _, m := range out {
		if ModelAliasCanonical(m) == "kimi-k3" {
			k3 = append(k3, m)
		}
	}
	if !modelsEqual(k3, []string{"kimi-k3"}) || !containsString(out, "kimi-for-coding") {
		t.Fatalf("one k3 row led by the curated slug: %v", out)
	}
}

func TestK3LiveDiscoveryScopedToCodingEndpoint(t *testing.T) {
	ctx := modelsTestHome(t)
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case "https://api.kimi.com/coding/v1/models":
			return modelsJSONResponse(req, 200, map[string]any{"data": []any{map[string]any{"id": "k3"}}}), nil
		case "https://api.moonshot.ai/v1/models":
			return modelsJSONResponse(req, 200, map[string]any{"data": []any{map[string]any{"id": "K3"}, map[string]any{"id": "kimi-k2.6"}}}), nil
		case "https://example.invalid/v1/models":
			return modelsJSONResponse(req, 200, map[string]any{"data": []any{map[string]any{"id": "k3"}, map[string]any{"id": "kimi-k2.6"}}}), nil
		}
		return nil, fmt.Errorf("unexpected Kimi models URL: %s", req.URL)
	})
	run := func(key, base string) []string {
		apiKeyCredentialsFn = func(context.Context, string) (string, string) { return key, base }
		return ProviderModelIDs(ctx, "kimi-coding", false).IDs
	}
	coding := run("sk-kimi-test", "https://api.kimi.com/coding")
	if len(coding) == 0 || coding[0] != "kimi-k3" {
		t.Fatalf("curated slug leads: %v", coding)
	}
	for _, rows := range [][]string{coding, run("legacy-test", "https://api.moonshot.ai/v1"), run("custom-test", "https://example.invalid/v1")} {
		for _, m := range rows {
			if strings.ToLower(m) == "k3" {
				t.Fatalf("bare k3 must never survive: %v", rows)
			}
		}
	}
}

func modelsWriteConfig(t *testing.T, ctx context.Context, doc map[string]any) {
	t.Helper()
	if err := config.SaveUser(home.Dir(ctx), doc); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

func TestCanonicalURLPreservesNativeCatalogButOtherPathsStayRelays(t *testing.T) {
	for _, provider := range []string{"openai-codex", "nous", "opencode-zen", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			ctx := modelsTestHome(t)
			profile, ok := ProfileFor(provider)
			if !ok || profile.BaseURL == "" {
				t.Fatalf("profile %s has no base url", provider)
			}
			canonical := profile.BaseURL
			var calls []string
			saved := providerCatalogFetchers[provider]
			providerCatalogFetchers[provider] = func(_ context.Context, slug string, _ bool) (CatalogRows, bool) {
				calls = append(calls, slug)
				return plainRows([]string{"native-catalog-model"}), true
			}
			t.Cleanup(func() { providerCatalogFetchers[provider] = saved })
			relayModelCatalogFn = func(context.Context, string, string) ([]string, bool) { return nil, false }
			scheme, rest, _ := strings.Cut(canonical, "://")
			host, path, _ := strings.Cut(rest, "/")
			shouted := strings.ToUpper(scheme) + "://" + strings.ToUpper(host) + ":443"
			if path != "" {
				shouted += "/" + path
			}
			shouted += "/"
			for _, c := range []struct {
				base     string
				expected []string
			}{{canonical, []string{provider}}, {shouted, []string{provider}}, {canonical + "/relay", nil}} {
				calls = nil
				modelsWriteConfig(t, ctx, map[string]any{"model": map[string]any{"provider": provider, "base_url": c.base}})
				catalog := ProviderModelIDs(ctx, provider, true)
				if !modelsEqual(calls, c.expected) || containsString(catalog.IDs, "native-catalog-model") != (len(c.expected) > 0) {
					t.Fatalf("%s: calls=%v catalog=%v", c.base, calls, catalog.IDs)
				}
			}
		})
	}
}

func TestRegistryEndpointIsCanonicalWhenTheProfileLeavesBaseURLEmpty(t *testing.T) {
	ctx := modelsTestHome(t)
	profile, ok := ProfileFor("tencent-tokenhub")
	if !ok {
		t.Fatal("tencent-tokenhub profile missing")
	}
	def, ok := lookupProviderDef("tencent-tokenhub")
	if !ok || def.BaseURL == "" {
		t.Fatal("registry endpoint missing")
	}
	modelsProfileForFn = func(name string) (Profile, bool) {
		if name == "tencent-tokenhub" {
			p := profile
			p.BaseURL = ""
			return p, true
		}
		return ProfileFor(name)
	}
	endpoint := def.BaseURL
	modelConfigDictFn = func(context.Context) map[string]any {
		return map[string]any{"provider": "tencent-tokenhub", "base_url": endpoint + "/"}
	}
	if got := configuredRelayBaseURL(ctx, "tencent-tokenhub"); got != "" {
		t.Fatalf("the registry endpoint is not a relay: %q", got)
	}
	modelConfigDictFn = func(context.Context) map[string]any {
		return map[string]any{"provider": "tencent-tokenhub", "base_url": endpoint + "/relay"}
	}
	if got := configuredRelayBaseURL(ctx, "tencent-tokenhub"); got != endpoint+"/relay" {
		t.Fatalf("other paths stay relays: %q", got)
	}
}

func modelsRelayConfig() map[string]any {
	return map[string]any{"provider": "deepseek", "base_url": "http://127.0.0.1:9001/deepseek/v1"}
}

func TestRelayBaseURLIsProbedForConfiguredProvider(t *testing.T) {
	ctx := modelsTestHome(t)
	modelConfigDictFn = func(context.Context) map[string]any { return modelsRelayConfig() }
	var calls [][2]string
	modelsProfileForFn = func(string) (Profile, bool) { return Profile{Name: "deepseek", AuthType: AuthTypeAPIKey}, true }
	profileFetchModelsFn = func(_ context.Context, _ Profile, apiKey, baseURL string, _ time.Duration) ([]string, bool) {
		calls = append(calls, [2]string{apiKey, baseURL})
		return []string{"relay-only-model"}, true
	}
	apiKeyCredentialsFn = func(context.Context, string) (string, string) { return "", "" }
	saved := providerCatalogFetchers
	providerCatalogFetchers = map[string]catalogFetcher{"deepseek": func(context.Context, string, bool) (CatalogRows, bool) {
		t.Fatal("canonical host touched")
		return CatalogRows{}, false
	}}
	t.Cleanup(func() { providerCatalogFetchers = saved })
	got := ProviderModelIDs(ctx, "deepseek", true)
	if !modelsEqual(got.IDs, []string{"relay-only-model"}) || len(calls) != 1 || calls[0] != [2]string{"", "http://127.0.0.1:9001/deepseek/v1"} {
		t.Fatalf("relay probe: %v %v", got.IDs, calls)
	}
	if configuredRelayBaseURL(ctx, "openai") != "" {
		t.Fatal("a base url for a different provider is ignored")
	}
}

func TestAFailedRelayProbeNeverFallsBackToTheVendorHost(t *testing.T) {
	for _, mode := range []string{"raise", "empty"} {
		t.Run(mode, func(t *testing.T) {
			ctx := modelsTestHome(t)
			modelConfigDictFn = func(context.Context) map[string]any { return modelsRelayConfig() }
			modelsProfileForFn = func(string) (Profile, bool) { return Profile{Name: "deepseek", AuthType: AuthTypeAPIKey}, true }
			profileFetchModelsFn = func(context.Context, Profile, string, string, time.Duration) ([]string, bool) {
				if mode == "raise" {
					return nil, false
				}
				return []string{}, true
			}
			apiKeyCredentialsFn = func(context.Context, string) (string, string) { return "", "" }
			egress := func(what string) {
				t.Fatalf("vendor host touched after a failed relay probe: %s", what)
			}
			saved := providerCatalogFetchers
			providerCatalogFetchers = map[string]catalogFetcher{"deepseek": func(context.Context, string, bool) (CatalogRows, bool) {
				egress("fetcher")
				return CatalogRows{}, false
			}}
			t.Cleanup(func() { providerCatalogFetchers = saved })
			profileLiveCatalogFn = func(context.Context, string) (CatalogRows, bool) { egress("profile"); return CatalogRows{}, false }
			mergeWithModelsDevFn = func(context.Context, string, []string) []string { egress("models.dev"); return nil }
			staticCatalogFn = func(_ context.Context, provider string) []string {
				if provider == "deepseek" {
					return []string{"curated-local"}
				}
				return nil
			}
			if got := ProviderModelIDs(ctx, "deepseek", false); !modelsEqual(got.IDs, []string{"curated-local"}) || !got.Fallback {
				t.Fatalf("degrades to the local curated placeholder: %+v", got)
			}
		})
	}
}

func TestAzureFoundryPickerCatalog(t *testing.T) {
	ctx := modelsTestHome(t)
	var seen [2]string
	azureFoundryRuntimeFn = func(context.Context, string, map[string]any) (*Runtime, error) {
		return &Runtime{BaseURL: "https://r.openai.azure.com/openai/v1/", APIKey: "k"}, nil
	}
	azureProbeOpenAIModelsFn = func(_ context.Context, baseURL, apiKey string, _ func(context.Context) (string, error)) (bool, []string) {
		seen = [2]string{baseURL, apiKey}
		return true, []string{"gpt-5.4", "kimi-k2.6"}
	}
	if got := ProviderModelIDs(ctx, "azure-foundry", true); !modelsEqual(got.IDs, []string{"gpt-5.4", "kimi-k2.6"}) {
		t.Fatalf("live resource catalog: %v", got.IDs)
	}
	if seen != [2]string{"https://r.openai.azure.com/openai/v1", "k"} {
		t.Fatalf("probe target: %v", seen)
	}
	azureProbeOpenAIModelsFn = func(context.Context, string, string, func(context.Context) (string, error)) (bool, []string) {
		return false, nil
	}
	azureFoundryRuntimeFn = func(context.Context, string, map[string]any) (*Runtime, error) {
		return &Runtime{BaseURL: "https://r.services.ai.azure.com/anthropic", APIKey: "k"}, nil
	}
	if got := ProviderModelIDs(ctx, "azure-foundry", true); len(got.IDs) != 0 {
		t.Fatalf("probe miss keeps the empty static catalog: %v", got.IDs)
	}
	azureFoundryRuntimeFn = func(context.Context, string, map[string]any) (*Runtime, error) {
		return nil, errors.New("Azure Foundry requires a base URL")
	}
	if got := ProviderModelIDs(ctx, "azure-foundry", true); len(got.IDs) != 0 {
		t.Fatalf("resolver error keeps the empty static catalog: %v", got.IDs)
	}
}

func TestDiskCacheFingerprintTracksTheConfiguredAzureResource(t *testing.T) {
	ctx := modelsTestHome(t)
	modelConfigDictFn = func(context.Context) map[string]any {
		return map[string]any{"provider": "azure-foundry", "base_url": "https://a.openai.azure.com/openai/v1"}
	}
	fpA := credentialFingerprint(ctx, "azure-foundry")
	modelConfigDictFn = func(context.Context) map[string]any {
		return map[string]any{"provider": "azure-foundry", "base_url": "https://b.openai.azure.com/openai/v1"}
	}
	if credentialFingerprint(ctx, "azure-foundry") == fpA {
		t.Fatal("a resource switch must change the fingerprint")
	}
}

var modelsPagedModels = func() []string {
	out := make([]string, 55)
	for i := range out {
		out[i] = fmt.Sprintf("claude-fake-%03d", i)
	}
	return out
}()

// Cursor paginated Anthropic models server with a page cap and an optional stuck cursor
func modelsPagedServer(t *testing.T, pageCap int, repeatCursor bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(404)
			return
		}
		limit := 20
		if v := r.URL.Query().Get("limit"); v != "" {
			limit, _ = strconv.Atoi(v)
		}
		if limit > pageCap {
			limit = pageCap
		}
		after := r.URL.Query().Get("after_id")
		start := 0
		for i, m := range modelsPagedModels {
			if m == after {
				start = i + 1
			}
		}
		end := start + limit
		if end > len(modelsPagedModels) {
			end = len(modelsPagedModels)
		}
		page := modelsPagedModels[start:end]
		var lastID any
		if len(page) > 0 {
			lastID = page[len(page)-1]
		}
		if repeatCursor && after != "" {
			lastID = after
		}
		var data []any
		for _, m := range page {
			data = append(data, map[string]any{"id": m, "type": "model"})
		}
		body := map[string]any{"data": data, "has_more": repeatCursor || end < len(modelsPagedModels), "last_id": lastID}
		raw, _ := json.Marshal(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFetchAnthropicModelsPagination(t *testing.T) {
	ctx := modelsTestHome(t)
	modelsTransport = nil
	got, ok := fetchAnthropicModels(ctx, 0, modelsPagedServer(t, 20, false), "sk-ant-api-test")
	if !ok || len(got) != len(modelsPagedModels) {
		t.Fatalf("follows the cursor across pages: ok=%v n=%d", ok, len(got))
	}
	got, ok = fetchAnthropicModels(ctx, 0, modelsPagedServer(t, 1000, false), "sk-ant-api-test")
	if !ok || len(got) != len(modelsPagedModels) {
		t.Fatalf("single page: ok=%v n=%d", ok, len(got))
	}
	got, ok = fetchAnthropicModels(ctx, 0, modelsPagedServer(t, 20, true), "sk-ant-api-test")
	if !ok || len(got) == 0 || len(got) > len(modelsPagedModels) {
		t.Fatalf("repeated cursor terminates: ok=%v n=%d", ok, len(got))
	}
}

func TestFetchAnthropicModelsRetriesWithoutThe1MBetaAndSorts(t *testing.T) {
	ctx := modelsTestHome(t)
	anthropicCommonBetas = append(cloneStrings(anthropicCommonBetas), anthropicContext1MBeta)
	t.Cleanup(func() { anthropicCommonBetas = anthropicCommonBetas[:len(anthropicCommonBetas)-1] })
	token := "sk-ant-oat01-" + strings.Repeat("x", 20)
	catalog := map[string]any{"data": []any{map[string]any{"id": "claude-haiku-9"}, map[string]any{"id": "claude-sonnet-9"}, map[string]any{"id": "claude-opus-9"}, map[string]any{"id": "claude-opus-9"}}}
	var betas []string
	serve := func(rejection string) {
		betas = nil
		modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
			betas = append(betas, req.Header.Get("anthropic-beta"))
			if strings.Contains(req.Header.Get("anthropic-beta"), anthropicContext1MBeta) {
				return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(rejection)), Request: req}, nil
			}
			return modelsJSONResponse(req, 200, catalog), nil
		})
	}
	serve("")
	if got, ok := fetchAnthropicModels(ctx, 0, "", token); ok || len(betas) != 1 {
		t.Fatalf("an empty 400 body is not the long context rejection: %v %v", got, betas)
	}
	serve("The long context beta is not yet available for this subscription.")
	got, ok := fetchAnthropicModels(ctx, 0, "", token)
	if !ok || !modelsEqual(got, []string{"claude-opus-9", "claude-sonnet-9", "claude-haiku-9"}) {
		t.Fatalf("retry without the 1M beta then tier sort: ok=%v %v", ok, got)
	}
	if len(betas) != 2 || !strings.Contains(betas[0], anthropicContext1MBeta) || strings.Contains(betas[1], anthropicContext1MBeta) || !strings.Contains(betas[1], anthropicOAuthOnlyBetas[0]) {
		t.Fatalf("beta headers: %v", betas)
	}
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("x-api-key") != "sk-ant-api-key" || req.Header.Get("anthropic-beta") != "" {
			t.Fatalf("api keys use x-api-key without betas: %v", req.Header)
		}
		return modelsJSONResponse(req, 200, catalog), nil
	})
	if got, ok = fetchAnthropicModels(ctx, 0, "", "sk-ant-api-key"); !ok || len(got) != 3 {
		t.Fatalf("api key path: %v", got)
	}
	if _, ok = fetchAnthropicModels(ctx, 0, "", ""); ok {
		t.Fatal("no credential anywhere means none")
	}
}

func TestAnthropicModelsURL(t *testing.T) {
	if got := anthropicModelsURL("", ""); got != "https://api.anthropic.com/v1/models?limit=1000" {
		t.Fatalf("default url: %q", got)
	}
	if got := anthropicModelsURL("https://proxy.example/v1/", "abc"); got != "https://proxy.example/v1/models?after_id=abc&limit=1000" {
		t.Fatalf("v1 base continues after a cursor: %q", got)
	}
	seen := map[string]bool{}
	if anthropicNextCursor(map[string]any{"has_more": true, "last_id": "x"}, seen) != "x" || anthropicNextCursor(map[string]any{"has_more": true, "last_id": "x"}, seen) != "" || anthropicNextCursor(map[string]any{"has_more": false, "last_id": "y"}, seen) != "" {
		t.Fatal("cursor rules")
	}
}

func TestProbeAPIModelsNegativeCache(t *testing.T) {
	ctx := modelsTestHome(t)
	timeoutErr := func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", Err: context.DeadlineExceeded}
	}
	modelsTransport = modelsFakeTransport(timeoutErr)
	r1 := ProbeAPIModels(ctx, "", "https://blackhole.invalid/v1", time.Second, "", nil)
	var calls []string
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.String())
		return timeoutErr(req)
	})
	r2 := ProbeAPIModels(ctx, "", "https://blackhole.invalid/v1/", time.Second, "", nil)
	if r1.Found || r2.Found || len(calls) != 0 || r2.ProbedURL != "https://blackhole.invalid/v1/models" {
		t.Fatalf("repeat failure inside the TTL skips the network: %+v %+v %v", r1, r2, calls)
	}
	ResetProviderModelsCaches()
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: http.NoBody, Request: req}, nil
	})
	if ProbeAPIModels(ctx, "bad-key", "https://reachable.invalid/v1", time.Second, "", nil).Found {
		t.Fatal("401 is not a catalog")
	}
	probeNegMu.Lock()
	_, cached := probeNegCache["reachable.invalid:443"]
	probeNegMu.Unlock()
	if cached {
		t.Fatal("an answering host is never cached as unreachable")
	}
	old := time.Now().Add(-probeNegTTL - time.Second)
	probeNegMu.Lock()
	probeNegCache["blackhole.invalid:443"] = old
	probeNegMu.Unlock()
	modelsTransport = modelsFakeTransport(timeoutErr)
	if ProbeAPIModels(ctx, "", "https://blackhole.invalid/v1", time.Second, "", nil).Found {
		t.Fatal("still unreachable")
	}
	probeNegMu.Lock()
	rerecorded := probeNegCache["blackhole.invalid:443"].After(old)
	probeNegCache["blackhole.invalid:443"] = old
	probeNegMu.Unlock()
	if !rerecorded {
		t.Fatal("expiry re probes and re records")
	}
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return modelsJSONResponse(req, 200, map[string]any{"data": []any{map[string]any{"id": "m1"}}}), nil
	})
	ok := ProbeAPIModels(ctx, "", "https://blackhole.invalid/v1", time.Second, "", nil)
	probeNegMu.Lock()
	_, still := probeNegCache["blackhole.invalid:443"]
	probeNegMu.Unlock()
	if !ok.Found || !modelsEqual(ok.Models, []string{"m1"}) || still || ok.ResolvedBaseURL != "https://blackhole.invalid/v1" || ok.SuggestedBaseURL != "https://blackhole.invalid" {
		t.Fatalf("recovery clears the entry: %+v still=%v", ok, still)
	}
}

func TestProbeAPIModelsHeadersAndFallbackCandidate(t *testing.T) {
	ctx := modelsTestHome(t)
	var seen []*http.Request
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req)
		if req.URL.Path == "/models" {
			return &http.Response{StatusCode: 404, Body: http.NoBody, Request: req}, nil
		}
		return modelsJSONResponse(req, 200, map[string]any{"data": []any{map[string]any{"id": "chat-a"}, map[string]any{"id": "img", "type": "image"}}}), nil
	})
	res := ProbeAPIModels(ctx, "key", "https://host.example", time.Second, string(ModeAnthropicMessages), map[string]string{"X-Tenant": "t"})
	if !res.Found || !res.UsedFallback || res.ResolvedBaseURL != "https://host.example/v1" || res.SuggestedBaseURL != "https://host.example" || res.ProbedURL != "https://host.example/v1/models" {
		t.Fatalf("fallback candidate: %+v", res)
	}
	if len(seen) != 2 || seen[0].Header.Get("x-api-key") != "key" || seen[0].Header.Get("anthropic-version") == "" || seen[0].Header.Get("Authorization") != "" || seen[0].Header.Get("X-Tenant") != "t" {
		t.Fatalf("anthropic mode headers: %v", seen[0].Header)
	}
	if ProbeAPIModels(ctx, "", "", time.Second, "", nil).Found {
		t.Fatal("empty base url probes nothing")
	}
	ids, ok := FetchAPIModels(ctx, "key", "https://host.example/v1", time.Second, "", nil)
	if !ok || !modelsEqual(ids, []string{"chat-a"}) || seen[len(seen)-1].Header.Get("Authorization") != "Bearer key" {
		t.Fatalf("bearer mode: %v %v", ids, seen[len(seen)-1].Header)
	}
}

func TestProbeAPIModelsProfileHookOwnsTheProbe(t *testing.T) {
	ctx := modelsTestHome(t)
	registerTestProfile(t, Profile{Name: "acme-host", Hostnames: []string{"acme-host.example"}, ProbeModelsHook: func(_ context.Context, baseURL, apiKey string, _ time.Duration) *ModelsProbe {
		if apiKey != "k" {
			t.Fatalf("hook receives the key: %q", apiKey)
		}
		return &ModelsProbe{IDs: []string{"owned"}, Listed: true, ProbedURL: baseURL + "/models", ResolvedBaseURL: baseURL}
	}})
	res := ProbeAPIModels(ctx, "k", "https://acme-host.example/v1/", time.Second, "", nil)
	if !res.Found || !modelsEqual(res.Models, []string{"owned"}) || res.ProbedURL != "https://acme-host.example/v1/models" || res.ResolvedBaseURL != "https://acme-host.example/v1" {
		t.Fatalf("profile hook answer expected: %+v", res)
	}
	RegisterProvider(Profile{Name: "acme-host", Hostnames: []string{"acme-host.example"}, ProbeModelsHook: func(context.Context, string, string, time.Duration) *ModelsProbe {
		return nil
	}})
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return modelsJSONResponse(req, 200, map[string]any{"data": []any{map[string]any{"id": "generic"}}}), nil
	})
	if res = ProbeAPIModels(ctx, "k", "https://acme-host.example/v1", time.Second, "", nil); !res.Found || !modelsEqual(res.Models, []string{"generic"}) {
		t.Fatalf("a declining hook falls through to the generic probe: %+v", res)
	}
}

func modelsCacheRow(models []string, age time.Duration, fp string) map[string]any {
	ids := make([]any, 0, len(models))
	for _, m := range models {
		ids = append(ids, m)
	}
	return map[string]any{"fp": fp, "at": float64(time.Now().Add(-age).UnixNano()) / 1e9, "models": ids}
}

// Seams a cached fetch test shares, every live fetch records a call
type modelsCachedFetchFixture struct {
	saved map[string]any
	live  int
	spawn []string
}

func modelsCachedFetch(t *testing.T, cache map[string]any, fp string, liveResult []string, liveOK bool) *modelsCachedFetchFixture {
	t.Helper()
	f := &modelsCachedFetchFixture{}
	loadProviderModelsCacheFn = func(context.Context) map[string]any {
		out := map[string]any{}
		for k, v := range cache {
			out[k] = v
		}
		return out
	}
	customEndpointFingerprintFn = func(any, string, map[string]string) string { return fp }
	saveProviderModelsCacheFn = func(_ context.Context, data map[string]any) { f.saved = data }
	fetchAPIModelsFn = func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		f.live++
		return liveResult, liveOK
	}
	spawnSWRRefreshFn = func(_ context.Context, key string, _ func() (map[string]any, bool)) { f.spawn = append(f.spawn, key) }
	return f
}

const modelsGW = "custom:https://gw.example.com/v1#fp"

func TestCachedFetchAPIModelsContract(t *testing.T) {
	ctx := modelsTestHome(t)
	f := modelsCachedFetch(t, map[string]any{modelsGW: modelsCacheRow([]string{"m1", "m2"}, 10*time.Second, "fp")}, "fp", nil, false)
	out, ok := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1"})
	if !ok || !modelsEqual(out.IDs, []string{"m1", "m2"}) || f.live != 0 || f.saved != nil {
		t.Fatalf("fresh entry served without a live fetch: %+v live=%d", out, f.live)
	}
	out, ok = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "HTTPS://GW.example.com/v1/"})
	if !ok || !modelsEqual(out.IDs, []string{"m1", "m2"}) || f.live != 0 {
		t.Fatal("cache key normalizes case and the trailing slash")
	}

	f = modelsCachedFetch(t, map[string]any{modelsGW: modelsCacheRow([]string{"old"}, providerModelsStaleServeMax+time.Minute, "fp")}, "fp", []string{"fresh-a", "fresh-b"}, true)
	out, ok = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1", TTL: time.Hour})
	row, _ := asMap(f.saved[modelsGW])
	if !ok || !modelsEqual(out.IDs, []string{"fresh-a", "fresh-b"}) || f.live != 1 || row == nil || row["fp"] != "fp" || !modelsEqual(entryModels(row), []string{"fresh-a", "fresh-b"}) {
		t.Fatalf("expired entry triggers a persisted live fetch: %+v saved=%v", out, f.saved)
	}

	f = modelsCachedFetch(t, map[string]any{"custom:https://gw.example.com/v1#old-fp": modelsCacheRow([]string{"old-key-models"}, 10*time.Second, "old-fp")}, "new-fp", []string{"new-key-models"}, true)
	out, _ = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-new-key", BaseURL: "https://gw.example.com/v1"})
	if !modelsEqual(out.IDs, []string{"new-key-models"}) || f.live != 1 {
		t.Fatal("a rotated key busts the cache even when fresh")
	}

	f = modelsCachedFetch(t, map[string]any{modelsGW: modelsCacheRow([]string{"stale-but-fresh"}, 5*time.Second, "fp")}, "fp", []string{"forced-live"}, true)
	out, _ = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1", ForceRefresh: true})
	if !modelsEqual(out.IDs, []string{"forced-live"}) || f.live != 1 {
		t.Fatal("force refresh bypasses a fresh cache")
	}

	f = modelsCachedFetch(t, map[string]any{modelsGW: modelsCacheRow([]string{"last-known-good"}, 99999*time.Second, "fp")}, "fp", nil, false)
	out, ok = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1"})
	if !ok || !modelsEqual(out.IDs, []string{"last-known-good"}) || f.saved != nil {
		t.Fatal("live failure falls back to the stale same fingerprint entry")
	}

	f = modelsCachedFetch(t, map[string]any{}, "fp", nil, false)
	if _, ok = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1"}); ok || f.saved != nil {
		t.Fatal("live failure with no entry returns the live value")
	}

	f = modelsCachedFetch(t, map[string]any{}, "fp", []string{}, true)
	out, ok = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1"})
	if !ok || len(out.IDs) != 0 || f.saved != nil {
		t.Fatal("an empty live result is returned but never persisted")
	}

	f = modelsCachedFetch(t, map[string]any{}, "fp", []string{"x"}, true)
	loadProviderModelsCacheFn = func(context.Context) map[string]any { t.Fatal("blank base url never touches the cache"); return nil }
	out, ok = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: ""})
	if !ok || !modelsEqual(out.IDs, []string{"x"}) || f.live != 1 {
		t.Fatal("blank base url calls straight through")
	}

	f = modelsCachedFetch(t, map[string]any{modelsGW: map[string]any{"fp": "fp", "at": "yesterday", "models": []any{"corrupt-row"}}}, "fp", []string{"live-models"}, true)
	out, _ = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1"})
	if !modelsEqual(out.IDs, []string{"live-models"}) || f.live != 1 {
		t.Fatal("a corrupt at field degrades to a live fetch")
	}

	f = modelsCachedFetch(t, map[string]any{modelsGW: modelsCacheRow([]string{"stale-ok"}, 2*time.Hour, "fp")}, "fp", nil, false)
	out, ok = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1", TTL: time.Hour})
	if !ok || !modelsEqual(out.IDs, []string{"stale-ok"}) || f.live != 0 || !modelsEqual(f.spawn, []string{modelsGW}) {
		t.Fatalf("stale window serves and refreshes off thread: %+v spawn=%v", out, f.spawn)
	}
	f = modelsCachedFetch(t, map[string]any{modelsGW: modelsCacheRow([]string{"ancient"}, providerModelsStaleServeMax+time.Minute, "fp")}, "fp", []string{"fresh"}, true)
	out, _ = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1"})
	if !modelsEqual(out.IDs, []string{"fresh"}) || f.live != 1 || len(f.spawn) != 0 {
		t.Fatal("beyond the stale window blocks on a live fetch")
	}
}

func TestCachedFetchAPIModelsCacheOnly(t *testing.T) {
	ctx := modelsTestHome(t)
	call := func(cache map[string]any, fp string, opts CachedFetchOptions) (CatalogRows, bool, *modelsCachedFetchFixture) {
		f := modelsCachedFetch(t, cache, fp, nil, false)
		fetchAPIModelsFn = func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
			t.Fatal("the caller's thread never blocks on the network")
			return nil, false
		}
		opts.APIKey, opts.BaseURL, opts.CacheOnly = "sk-key", "https://gw.example.com/v1", true
		out, ok := CachedFetchAPIModels(ctx, opts)
		if f.saved != nil {
			t.Fatal("cache only never saves")
		}
		return out, ok, f
	}
	if out, ok, f := call(map[string]any{modelsGW: modelsCacheRow([]string{"m1", "m2"}, 10*time.Second, "fp")}, "fp", CachedFetchOptions{}); !ok || !modelsEqual(out.IDs, []string{"m1", "m2"}) || len(f.spawn) != 0 {
		t.Fatal("fresh entry is served")
	}
	if out, ok, f := call(map[string]any{modelsGW: modelsCacheRow([]string{"m1", "m2"}, providerModelsCacheTTL+time.Minute, "fp")}, "fp", CachedFetchOptions{}); !ok || !modelsEqual(out.IDs, []string{"m1", "m2"}) || len(f.spawn) != 1 {
		t.Fatal("past TTL inside the stale window is served and revalidated")
	}
	if _, ok, _ := call(map[string]any{modelsGW: modelsCacheRow([]string{"ancient"}, providerModelsStaleServeMax+time.Minute, "fp")}, "fp", CachedFetchOptions{}); ok {
		t.Fatal("beyond the stale window is a miss")
	}
	if _, ok, _ := call(map[string]any{}, "fp", CachedFetchOptions{}); ok {
		t.Fatal("empty cache is a miss")
	}
	if _, ok, _ := call(map[string]any{"custom:https://gw.example.com/v1#old-fp": modelsCacheRow([]string{"old"}, 10*time.Second, "old-fp")}, "new-fp", CachedFetchOptions{}); ok {
		t.Fatal("rotated credentials are a miss")
	}
	if _, ok, _ := call(map[string]any{modelsGW: modelsCacheRow([]string{"m1"}, 10*time.Second, "fp")}, "fp", CachedFetchOptions{ForceRefresh: true}); ok {
		t.Fatal("cache only outranks force refresh")
	}
	fetchAPIModelsFn = func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		t.Fatal("missing base url never fetches")
		return nil, false
	}
	if _, ok := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", CacheOnly: true}); ok {
		t.Fatal("missing base url is a miss")
	}
	fpA := customEndpointFingerprint("sk-key", "", map[string]string{"X-Tenant": "a"})
	fpB := customEndpointFingerprint("sk-key", "", map[string]string{"X-Tenant": "b"})
	if fpA == fpB || fpA != customEndpointFingerprint("sk-key", "", map[string]string{"X-Tenant": "a"}) {
		t.Fatal("fingerprint reacts to headers only")
	}
	if customEndpointFingerprint(&CommandTokenSource{Command: "op read x"}, "", nil) != customEndpointFingerprint(&CommandTokenSource{Command: "op read x"}, "", nil) {
		t.Fatal("command token sources fingerprint by command identity")
	}
}

func TestCachedFetchAPIModelsDiskRoundTrip(t *testing.T) {
	ctx := modelsTestHome(t)
	var calls [][2]string
	fetchAPIModelsFn = func(_ context.Context, apiKey, baseURL string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
		calls = append(calls, [2]string{apiKey, baseURL})
		return []string{"disk-cached-model"}, true
	}
	first, _ := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1"})
	second, _ := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://gw.example.com/v1"})
	if !modelsEqual(first.IDs, []string{"disk-cached-model"}) || !modelsEqual(second.IDs, first.IDs) || len(calls) != 1 {
		t.Fatalf("second open is served from disk: %v", calls)
	}

	fetchAPIModelsFn = func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		return []string{"custom-endpoint-model"}, true
	}
	providerModelIDsFn = func(context.Context, string, bool) CatalogRows {
		return plainRows([]string{"openrouter-curated-model"})
	}
	CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-key", BaseURL: "https://openrouter.ai/v1"})
	CachedProviderModelIDs(ctx, "openrouter", CachedModelsOptions{})
	cache := loadProviderModelsCache(ctx)
	var customRows [][]string
	for k, v := range cache {
		if strings.HasPrefix(k, "custom:https://openrouter.ai/v1#") {
			row, _ := asMap(v)
			customRows = append(customRows, entryModels(row))
		}
	}
	orRow, _ := asMap(cache["openrouter"])
	if len(customRows) != 1 || !modelsEqual(customRows[0], []string{"custom-endpoint-model"}) || !modelsEqual(entryModels(orRow), []string{"openrouter-curated-model"}) {
		t.Fatalf("custom keys never collide with provider slugs: %v", cache)
	}

	fetchAPIModelsFn = func(_ context.Context, key, _ string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
		return []string{"models-for-" + key}, true
	}
	const proxy = "https://proxy.example.com/v1"
	a, _ := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-A", BaseURL: proxy})
	b, _ := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-B", BaseURL: proxy})
	if !modelsEqual(a.IDs, []string{"models-for-sk-A"}) || !modelsEqual(b.IDs, []string{"models-for-sk-B"}) {
		t.Fatal("distinct keys on one url")
	}
	fetchAPIModelsFn = func(context.Context, string, string, time.Duration, string, map[string]string) ([]string, bool) {
		t.Fatal("cache miss")
		return nil, false
	}
	a, _ = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-A", BaseURL: proxy, CacheOnly: true})
	b, _ = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: "sk-B", BaseURL: proxy, CacheOnly: true})
	if !modelsEqual(a.IDs, []string{"models-for-sk-A"}) || !modelsEqual(b.IDs, []string{"models-for-sk-B"}) {
		t.Fatal("same url distinct credentials keep separate rows")
	}
}

func TestSWRRefreshWritesCustomEntryThroughSharedScaffolding(t *testing.T) {
	ctx := modelsTestHome(t)
	var mu sync.Mutex
	saved := map[string]any{}
	loadProviderModelsCacheFn = func(context.Context) map[string]any { return map[string]any{} }
	saveProviderModelsCacheFn = func(_ context.Context, data map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		for k, v := range data {
			saved[k] = v
		}
	}
	spawnSWRRefresh(ctx, modelsGW, func() (map[string]any, bool) { return modelsCacheRow([]string{"refreshed"}, 0, "fp"), true })
	WaitProviderModelsRefreshes()
	mu.Lock()
	row, _ := asMap(saved[modelsGW])
	mu.Unlock()
	if !modelsEqual(entryModels(row), []string{"refreshed"}) {
		t.Fatalf("refresh persisted: %v", saved)
	}
	swrRefreshMu.Lock()
	inflight := swrRefreshInflight[swrKey{home.Key(home.Dir(ctx)), modelsGW}]
	swrRefreshMu.Unlock()
	if inflight {
		t.Fatal("inflight key released")
	}
}

func modelsSWRFixture(t *testing.T, cache map[string]any, fp string, live []string) *modelsCachedFetchFixture {
	t.Helper()
	f := modelsCachedFetch(t, cache, fp, nil, false)
	credentialFingerprintFn = func(context.Context, string) string { return fp }
	providerModelIDsFn = func(context.Context, string, bool) CatalogRows {
		f.live++
		return plainRows(live)
	}
	return f
}

func TestProviderModelsSWR(t *testing.T) {
	ctx := modelsTestHome(t)
	f := modelsSWRFixture(t, map[string]any{"openrouter": modelsCacheRow([]string{"m1"}, 10*time.Second, "fp")}, "fp", nil)
	if out := CachedProviderModelIDs(ctx, "openrouter", CachedModelsOptions{}); !modelsEqual(out, []string{"m1"}) || len(f.spawn) != 0 || f.live != 0 {
		t.Fatal("fresh entry served without refresh")
	}
	f = modelsSWRFixture(t, map[string]any{"openrouter": modelsCacheRow([]string{"m1", "m2"}, 2*time.Hour, "fp")}, "fp", nil)
	if out := CachedProviderModelIDs(ctx, "openrouter", CachedModelsOptions{}); !modelsEqual(out, []string{"m1", "m2"}) || !modelsEqual(f.spawn, []string{"openrouter"}) || f.live != 0 {
		t.Fatal("stale entry served immediately with a background refresh")
	}
	f = modelsSWRFixture(t, map[string]any{"openrouter": modelsCacheRow([]string{"ancient"}, providerModelsStaleServeMax+time.Minute, "fp")}, "fp", []string{"fresh"})
	if out := CachedProviderModelIDs(ctx, "openrouter", CachedModelsOptions{}); !modelsEqual(out, []string{"fresh"}) || len(f.spawn) != 0 || f.live != 1 {
		t.Fatal("too old entry blocks on a live fetch")
	}
	f = modelsSWRFixture(t, map[string]any{"openrouter": modelsCacheRow([]string{"old-key-models"}, 2*time.Hour, "old")}, "new", []string{"new-key-models"})
	if out := CachedProviderModelIDs(ctx, "openrouter", CachedModelsOptions{}); !modelsEqual(out, []string{"new-key-models"}) || len(f.spawn) != 0 {
		t.Fatal("credential rotation busts the stale entry")
	}
	f = modelsSWRFixture(t, map[string]any{"openrouter": modelsCacheRow([]string{"m1"}, 2*time.Hour, "fp")}, "fp", []string{"live"})
	if out := CachedProviderModelIDs(ctx, "openrouter", CachedModelsOptions{ForceRefresh: true}); !modelsEqual(out, []string{"live"}) || len(f.spawn) != 0 || f.live != 1 {
		t.Fatal("force refresh bypasses SWR")
	}
	for _, badAt := range []any{"yesterday", nil, true} {
		f = modelsSWRFixture(t, map[string]any{"openrouter": map[string]any{"fp": "fp", "at": badAt, "models": []any{"corrupt-row"}}}, "fp", []string{"live-model"})
		if out := CachedProviderModelIDs(ctx, "openrouter", CachedModelsOptions{}); !modelsEqual(out, []string{"live-model"}) || f.live != 1 {
			t.Fatalf("corrupt at %v falls back to a live fetch", badAt)
		}
	}
	f = modelsSWRFixture(t, map[string]any{}, "fp", []string{"fresh1", "fresh2"})
	spawnSWRRefreshFn = spawnSWRRefresh
	spawnSWRRefresh(ctx, "openrouter", nil)
	spawnSWRRefresh(ctx, "openrouter", nil)
	WaitProviderModelsRefreshes()
	row, _ := asMap(f.saved["openrouter"])
	if !modelsEqual(entryModels(row), []string{"fresh1", "fresh2"}) {
		t.Fatalf("SWR refresh writes the cache: %v", f.saved)
	}
	swrRefreshMu.Lock()
	n := len(swrRefreshInflight)
	swrRefreshMu.Unlock()
	if n != 0 {
		t.Fatal("inflight cleared on completion")
	}
}

func TestSWRRefreshDedupesInflight(t *testing.T) {
	ctx := modelsTestHome(t)
	block := make(chan struct{})
	var started []string
	var mu sync.Mutex
	refreshFor := func(key string) func() (map[string]any, bool) {
		return func() (map[string]any, bool) {
			mu.Lock()
			started = append(started, key)
			mu.Unlock()
			<-block
			return nil, false
		}
	}
	spawnSWRRefresh(ctx, "openrouter", refreshFor("openrouter"))
	spawnSWRRefresh(ctx, "openrouter", refreshFor("openrouter"))
	spawnSWRRefresh(ctx, "nous", refreshFor("nous"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(started)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(block)
	WaitProviderModelsRefreshes()
	sort.Strings(started)
	if !modelsEqual(started, []string{"nous", "openrouter"}) {
		t.Fatalf("one refresh per key: %v", started)
	}
}

func modelsCodexJWT(accountID, subject, nonce string, exp int64) string {
	seg := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	claims := map[string]any{"sub": subject, "nonce": nonce, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID}}
	if exp != 0 {
		claims["exp"] = exp
	}
	return seg(map[string]any{"alg": "none"}) + "." + seg(claims) + ".sig"
}

func TestCodexTokenRotationKeepsAccountGatedModelsForSamePrincipal(t *testing.T) {
	ctx := modelsTestHome(t)
	creds := map[string]any{}
	resolveCodexRuntimeCredentialsReadOnlyFn = func(PoolEnv) (map[string]any, error) {
		out := map[string]any{}
		for k, v := range creds {
			out[k] = v
		}
		return out, nil
	}
	setToken := func(token string) { creds["api_key"], creds["base_url"] = token, DefaultCodexBaseURL }
	seed := func(models []string) {
		storeCacheEntry(ctx, "openai-codex", cacheEntry(credentialFingerprint(ctx, "openai-codex"), models, time.Now()))
	}
	var spawn []string
	spawnSWRRefreshFn = func(_ context.Context, key string, _ func() (map[string]any, bool)) { spawn = append(spawn, key) }
	setToken(modelsCodexJWT("account-a", "user-a", "first", 0))
	astra := []string{"gpt-6-astra", "gpt-6-astra-900k"}
	seed(astra)
	setToken(modelsCodexJWT("account-a", "user-a", "rotated", 0))
	if got := CachedProviderModelIDs(ctx, "openai-codex", CachedModelsOptions{NonBlocking: true}); !modelsEqual(got, astra) || len(spawn) != 0 {
		t.Fatalf("rotation keeps the account catalog: %v spawn=%v", got, spawn)
	}
	setToken(modelsCodexJWT("account-a", "user-b", "new-member", 0))
	if got := CachedProviderModelIDs(ctx, "openai-codex", CachedModelsOptions{NonBlocking: true}); len(got) != 0 || !modelsEqual(spawn, []string{"openai-codex"}) {
		t.Fatalf("a different subject is a different boundary: %v spawn=%v", got, spawn)
	}
	spawn = nil
	seed(astra)
	setToken(modelsCodexJWT("account-b", "user-b", "new-account", 0))
	if got := CachedProviderModelIDs(ctx, "openai-codex", CachedModelsOptions{NonBlocking: true}); len(got) != 0 || !modelsEqual(spawn, []string{"openai-codex"}) {
		t.Fatalf("a different account is a different boundary: %v spawn=%v", got, spawn)
	}
	spawn = nil
	setToken(modelsCodexJWT("account-b", "user-b", "stale", time.Now().Unix()-60))
	seed([]string{"gpt-5.6-sol"})
	setToken(modelsCodexJWT("account-b", "user-b", "fresh", time.Now().Unix()+3600))
	if got := CachedProviderModelIDs(ctx, "openai-codex", CachedModelsOptions{NonBlocking: true}); len(got) != 0 || !modelsEqual(spawn, []string{"openai-codex"}) {
		t.Fatalf("an expired token row never outlives the refresh: %v spawn=%v", got, spawn)
	}
}

func TestCachedProviderModelIDsNonBlockingDropsAccountGatedRows(t *testing.T) {
	ctx := modelsTestHome(t)
	f := modelsSWRFixture(t, map[string]any{"openai-api": modelsCacheRow([]string{"gpt-5.5", "gpt-6-astra"}, 2*time.Hour, "fp")}, "fp", nil)
	if out := CachedProviderModelIDs(ctx, "openai-api", CachedModelsOptions{}); !modelsEqual(out, []string{"gpt-5.5", "gpt-6-astra"}) {
		t.Fatalf("stale rows are served whole: %v", out)
	}
	f = modelsSWRFixture(t, map[string]any{"openai-api": modelsCacheRow([]string{"gpt-5.5", "gpt-6-astra"}, providerModelsStaleServeMax+time.Minute, "fp")}, "fp", nil)
	if out := CachedProviderModelIDs(ctx, "openai-api", CachedModelsOptions{NonBlocking: true}); !modelsEqual(out, []string{"gpt-5.5"}) || !modelsEqual(f.spawn, []string{"openai-api"}) {
		t.Fatalf("non blocking read drops account gated rows and warms: %v", out)
	}
	if out := CachedProviderModelIDs(ctx, "", CachedModelsOptions{}); len(out) != 0 {
		t.Fatal("empty provider")
	}
	if normalizedCacheSlug("ollama") != "ollama" || normalizedCacheSlug("Claude") != "anthropic" {
		t.Fatal("cache slug normalization")
	}
}

func TestLiveResultEntryNeverReplacesARealCatalogWithAPlaceholder(t *testing.T) {
	existing := modelsCacheRow([]string{"real"}, time.Minute, "fp")
	if _, ok := liveResultEntry("fp", fallbackRows([]string{"curated"}), existing, time.Now()); ok {
		t.Fatal("a curated fallback keeps the existing real row")
	}
	entry, ok := liveResultEntry("fp", fallbackRows([]string{"curated"}), nil, time.Now())
	if !ok || entry["fallback"] != true {
		t.Fatal("stored fallbacks are flagged")
	}
	if tier := diskServeTier(entry, "fp", time.Now().Add(providerModelsFallbackTTL+time.Second), false, time.Hour); tier != "" {
		t.Fatalf("fallback rows expire on the short TTL: %q", tier)
	}
	if tier := diskServeTier(entry, "fp", time.Now(), false, time.Hour); tier != "fresh" {
		t.Fatalf("fresh fallback: %q", tier)
	}
	if tier := diskServeTier(modelsCacheRow([]string{}, 0, "fp"), "fp", time.Now(), true, time.Hour); tier != "fresh" {
		t.Fatalf("empty native catalogs are authoritative inside the TTL: %q", tier)
	}
	if tier := diskServeTier(modelsCacheRow([]string{}, 2*ollamaLocalModelsCacheTTL, "fp"), "fp", time.Now(), true, time.Hour); tier != "" {
		t.Fatalf("empty native catalogs never stale serve: %q", tier)
	}
}

func TestClearProviderModelsCache(t *testing.T) {
	ctx := modelsTestHome(t)
	storeCacheEntry(ctx, "openrouter", cacheEntry("fp", []string{"a"}, time.Now()))
	storeCacheEntry(ctx, "nous", cacheEntry("fp", []string{"b"}, time.Now()))
	invalidated := 0
	registerTestProfile(t, Profile{Name: "acme", InvalidateCatalogCacheHook: func() { invalidated++ }})
	ClearProviderModelsCache(ctx, "openrouter")
	cache := loadProviderModelsCache(ctx)
	if _, ok := cache["openrouter"]; ok || cache["nous"] == nil || invalidated != 1 {
		t.Fatalf("one row dropped: %v", cache)
	}
	ClearAllProviderModelsCache(ctx)
	if _, err := os.Stat(providerModelsCachePath(ctx)); !os.IsNotExist(err) || invalidated != 2 {
		t.Fatal("whole file removed")
	}
}

func TestListAvailableProviders(t *testing.T) {
	ctx := modelsTestHome(t)
	providerAuthStatusFn = func(_ context.Context, provider string) (bool, bool) { return provider == "nous", false }
	rows := ListAvailableProviders(ctx)
	byID := map[string]AvailableProvider{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if rows[len(rows)-1].ID != "custom" || !byID["nous"].Authenticated || byID["anthropic"].Authenticated || !containsString(byID["anthropic"].Aliases, "claude") {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestProfileFetchModelsDefault(t *testing.T) {
	ctx := modelsTestHome(t)
	var seen []string
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req.URL.String()+" "+req.Header.Get("Authorization")+" "+req.Header.Get("X-Extra"))
		return modelsJSONResponse(req, 200, map[string]any{"data": []any{map[string]any{"id": "m"}, map[string]any{"id": "gen", "type": "image"}}}), nil
	})
	p := Profile{Name: "acme", ModelListing: true, BaseURL: "https://acme.example/v1", ModelsURL: "https://acme.example/catalog", Headers: map[string]string{"X-Extra": "1"}}
	if ids, ok := profileFetchModels(ctx, p, "k", "", 0); !ok || !modelsEqual(ids, []string{"m"}) || seen[0] != "https://acme.example/catalog Bearer k 1" {
		t.Fatalf("models url wins when the caller base is not customised: %v %v", ids, seen)
	}
	if _, ok := profileFetchModels(ctx, p, "k", "https://proxy.example/v1/", 0); !ok || seen[1] != "https://proxy.example/v1/models Bearer k 1" {
		t.Fatalf("a customised base probes its own models path: %v", seen)
	}
	p.ModelListing = false
	if _, ok := profileFetchModels(ctx, p, "k", "", 0); ok {
		t.Fatal("no listing support")
	}
	hooked := Profile{Name: "hooked", ModelListing: true, BaseURL: "https://h.example/v1",
		FetchModelsHook: func(context.Context, string, string, time.Duration) ([]string, bool) { return []string{"hook"}, true }}
	if ids, _ := profileFetchModels(ctx, hooked, "", "", 0); !modelsEqual(ids, []string{"hook"}) {
		t.Fatal("hook override")
	}
}

func TestMergeProfileCatalog(t *testing.T) {
	ctx := modelsTestHome(t)
	p := Profile{Name: "acme", FallbackModels: []string{"fb-1"}}
	rows, ok := MergeProfileCatalog(ctx, "acme", p, nil)
	if !ok || !rows.Fallback || !modelsEqual(rows.IDs, []string{"fb-1"}) {
		t.Fatalf("empty live serves the fallback as a placeholder: %+v", rows)
	}
	if _, ok := MergeProfileCatalog(ctx, "acme", Profile{Name: "acme"}, nil); ok {
		t.Fatal("no fallback means none")
	}
	rows, _ = MergeProfileCatalog(ctx, "acme", p, []string{"live-1", "FB-1"})
	if !modelsEqual(rows.IDs, []string{"fb-1", "live-1"}) || rows.Fallback {
		t.Fatalf("curated first then live extras: %+v", rows)
	}
	rows, _ = MergeProfileCatalog(ctx, "opencode-zen", Profile{Name: "opencode-zen"}, []string{"live-z", "ox-alpha-free", staticCatalogModels("opencode-zen")[0]})
	if rows.IDs[0] != "live-z" || containsString(rows.IDs, "ox-alpha-free") {
		t.Fatalf("live first pickers lead with live rows and drop delisted ids: %v", rows.IDs)
	}
}

func TestCuratedModelsForProvider(t *testing.T) {
	ctx := modelsTestHome(t)
	providerModelIDsFn = func(context.Context, string, bool) CatalogRows { return CatalogRows{} }
	rows := CuratedModelsForProvider(ctx, "deepseek", false)
	if !modelsEqual(modelsIDs(rows), staticCatalogModels("deepseek")) {
		t.Fatalf("static fallback: %v", rows)
	}
	providerModelIDsFn = func(context.Context, string, bool) CatalogRows { return plainRows([]string{"live"}) }
	if rows = CuratedModelsForProvider(ctx, "deepseek", false); !modelsEqual(modelsIDs(rows), []string{"live"}) {
		t.Fatalf("live rows: %v", rows)
	}
}
