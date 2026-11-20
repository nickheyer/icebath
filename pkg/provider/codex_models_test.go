package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// JWT shaped stand in so the live catalog path treats the token as a ChatGPT credential
func codexTestJWT(subject string) string {
	enc := func(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
	payload, _ := json.Marshal(map[string]any{"sub": subject})
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".test-signature"
}

func codexProSlugs(ids []string) []string {
	var out []string
	for _, m := range ids {
		if strings.HasSuffix(strings.TrimSuffix(m, "-900k"), "-pro") {
			out = append(out, m)
		}
	}
	return out
}

func TestCodexCatalogNeverOffersChatGPTRejectedProSlugs(t *testing.T) {
	ctx := modelsTestHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	offline, fallback := GetCodexModelIDs(ctx, "", "")
	if !fallback {
		t.Fatal("offline result is a curated placeholder")
	}
	for _, m := range DefaultCodexModels {
		if !containsString(offline, m) {
			t.Fatalf("curated model %s missing from %v", m, offline)
		}
	}
	if len(codexProSlugs(offline)) != 0 {
		t.Fatalf("pro slugs leaked: %v", offline)
	}
	var templates []string
	for _, fc := range codexForwardCompatTemplates {
		for _, tpl := range fc.templates {
			if !containsString(templates, tpl) {
				templates = append(templates, tpl)
			}
		}
	}
	fetchCodexModelsFromAPIFn = func(context.Context, string, string) []string { return templates }
	live, fallback := GetCodexModelIDs(ctx, "codex-access-token", "")
	if fallback {
		t.Fatal("live discovery is not a placeholder")
	}
	for _, fc := range codexForwardCompatTemplates {
		if !containsString(live, fc.synthetic) {
			t.Fatalf("synthetic %s missing from %v", fc.synthetic, live)
		}
	}
	if len(codexProSlugs(live)) != 0 {
		t.Fatalf("pro slugs synthesized: %v", live)
	}
}

func TestPickerSynthesizes900kVariantsForVerifiedSlugs(t *testing.T) {
	ctx := modelsTestHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	ids, _ := GetCodexModelIDs(ctx, "", "")
	index := func(m string) int {
		for i, x := range ids {
			if x == m {
				return i
			}
		}
		return -1
	}
	for _, base := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.4"} {
		if index(base) < 0 || index(base+"-900k") != index(base)+1 {
			t.Fatalf("%s needs its 900k twin right after it: %v", base, ids)
		}
	}
	for _, m := range []string{"gpt-5.5-900k", "gpt-5.4-mini-900k", "gpt-5.3-codex-900k"} {
		if containsString(ids, m) {
			t.Fatalf("%s must not be synthesized: %v", m, ids)
		}
	}
	out := finalizeCodexModels([]string{"gpt-5.6-sol-pro", "gpt-5.6-nova"})
	if containsString(out, "gpt-5.6-sol-pro-900k") || containsString(out, "gpt-5.6-nova-900k") {
		t.Fatalf("pro and unknown slugs never gain a 900k variant: %v", out)
	}
}

func TestCatalogIdentityStaysStableForRotationButChangesWithRoute(t *testing.T) {
	ctx := modelsTestHome(t)
	creds := map[string]any{"api_key": modelsCodexJWT("account", "subject", "first", 0), "base_url": "https://first.example/codex/"}
	resolveCodexRuntimeCredentialsReadOnlyFn = func(PoolEnv) (map[string]any, error) {
		out := map[string]any{}
		for k, v := range creds {
			out[k] = v
		}
		return out, nil
	}
	first := CodexCatalogCredentialIdentity(ctx)
	if first == "missing" || !strings.HasSuffix(first, "\n"+NormalizeRouteBaseURL("https://first.example/codex")) {
		t.Fatalf("identity carries the route: %q", first)
	}
	creds["api_key"] = modelsCodexJWT("account", "subject", "rotated", 0)
	creds["base_url"] = "https://first.example/codex"
	if CodexCatalogCredentialIdentity(ctx) != first {
		t.Fatal("token rotation keeps the identity")
	}
	creds["base_url"] = "https://second.example/codex"
	if CodexCatalogCredentialIdentity(ctx) == first {
		t.Fatal("a different route changes the identity")
	}
	creds["api_key"] = ""
	if CodexCatalogCredentialIdentity(ctx) != "missing" {
		t.Fatal("no token is missing")
	}
	creds["api_key"] = modelsCodexJWT("acct", "sub", "n", 1)
	if got := CodexCatalogCredentialIdentity(ctx); !strings.HasPrefix(got, "expired\n") {
		t.Fatalf("expired token is its own state: %q", got)
	}
	creds["api_key"] = modelsCodexJWT("acct", "sub", "n", 0)
	if got := CodexCatalogCredentialIdentity(ctx); !strings.HasPrefix(got, "acct/sub\n") {
		t.Fatalf("principal identity: %q", got)
	}
}

func codexGatedCatalogTransport(seen *[]string) modelsFakeTransport {
	return func(req *http.Request) (*http.Response, error) {
		*seen = append(*seen, req.URL.String())
		version := req.URL.Query().Get("client_version")
		models := []any{map[string]any{"slug": "gpt-5.6-sol", "visibility": "list", "context_window": 272000}}
		if version != "0.0.0" {
			models = append(models, map[string]any{"slug": "gpt-6-sol", "visibility": "list", "context_window": 272000})
		}
		return modelsJSONResponse(req, 200, map[string]any{"models": models}), nil
	}
}

func TestFetchFromAPIKeepsSupportedInAPIFalseModels(t *testing.T) {
	ctx := modelsTestHome(t)
	metadataTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		return modelsJSONResponse(req, 200, map[string]any{"models": []any{
			map[string]any{"slug": "gpt-5.5", "priority": 0, "supported_in_api": true},
			map[string]any{"slug": "gpt-5.3-codex-spark", "priority": 7, "supported_in_api": false},
			map[string]any{"slug": "gpt-5-internal", "priority": 99, "visibility": "hidden"},
		}}), nil
	})
	t.Cleanup(func() { metadataTransport = nil })
	models := fetchCodexModelsFromAPI(ctx, codexTestJWT("acct"), "")
	if !containsString(models, "gpt-5.5") || !containsString(models, "gpt-5.3-codex-spark") || containsString(models, "gpt-5-internal") {
		t.Fatalf("supported_in_api is not a filter, visibility is: %v", models)
	}
}

func TestCatalogRequestsAskAsTheNewestClient(t *testing.T) {
	ctx := modelsTestHome(t)
	var seen []string
	metadataTransport = codexGatedCatalogTransport(&seen)
	t.Cleanup(func() { metadataTransport = nil })
	if models := fetchCodexModelsFromAPI(ctx, codexTestJWT("acct"), ""); !containsString(models, "gpt-6-sol") {
		t.Fatalf("newest client answer: %v", models)
	}
	if len(seen) != 1 {
		t.Fatalf("one request when the newest client answer is non empty: %v", seen)
	}
	parsed, _ := url.Parse(seen[0])
	if parsed.Host != "chatgpt.com" || parsed.Path != "/backend-api/codex/models" || parsed.Query().Get("client_version") == "0.0.0" {
		t.Fatalf("request shape: %s", seen[0])
	}
}

func TestGatewayKeyIsNeverSentToTheDirectCatalog(t *testing.T) {
	ctx := modelsTestHome(t)
	calls := 0
	metadataTransport = modelsFakeTransport(func(*http.Request) (*http.Response, error) {
		calls++
		t.Fatal("a gateway key must not reach any catalog endpoint")
		return nil, nil
	})
	t.Cleanup(func() { metadataTransport = nil })
	if got := fetchCodexModelsFromAPI(ctx, "dummy-gateway-pool-key", ""); len(got) != 0 || calls != 0 {
		t.Fatalf("discovery declines: %v", got)
	}
}

func TestPickerCatalogHonoursTheCustomCodexBase(t *testing.T) {
	ctx := modelsTestHome(t)
	t.Setenv("ICEBATH_CODEX_BASE_URL", "https://codex-gw.example/backend-api/codex")
	var seen []string
	metadataTransport = codexGatedCatalogTransport(&seen)
	t.Cleanup(func() { metadataTransport = nil })
	if models := fetchCodexModelsFromAPI(ctx, codexTestJWT("acct"), ""); !containsString(models, "gpt-6-sol") || len(seen) == 0 {
		t.Fatalf("discovery must still run for a JWT credential: %v %v", models, seen)
	}
	for _, raw := range seen {
		parsed, _ := url.Parse(raw)
		if parsed.Host != "codex-gw.example" || parsed.Path != "/backend-api/codex/models" {
			t.Fatalf("gateway host expected: %s", raw)
		}
	}
	seen = nil
	fetchCodexModelsFromAPI(ctx, codexTestJWT("acct"), "https://other-gw.example/codex/")
	if len(seen) == 0 || !strings.HasPrefix(seen[0], "https://other-gw.example/codex/models") {
		t.Fatalf("the credential's own host wins over the env override: %v", seen)
	}
}

func TestAstraRequiresLiveCodexAccountDiscovery(t *testing.T) {
	ctx := modelsTestHome(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"gpt-6-astra\"\n"), 0o600)
	cache, _ := json.Marshal(map[string]any{"models": []any{
		map[string]any{"slug": "gpt-6-astra", "priority": 0},
		map[string]any{"slug": "openai/gpt-6-astra", "priority": 1},
		map[string]any{"slug": "gpt-6-astra-900k", "priority": 2},
		map[string]any{"slug": "openai/gpt-6-astra-900k", "priority": 3},
	}})
	os.WriteFile(filepath.Join(codexHome, "models_cache.json"), cache, 0o600)
	fetchCodexModelsFromAPIFn = func(context.Context, string, string) []string { return nil }
	ids, _ := GetCodexModelIDs(ctx, "stale-token", "")
	for _, m := range []string{"gpt-6-astra", "openai/gpt-6-astra", "gpt-6-astra-900k", "openai/gpt-6-astra-900k"} {
		if containsString(ids, m) {
			t.Fatalf("cached Astra must not manufacture entitlement: %v", ids)
		}
	}
	fetchCodexModelsFromAPIFn = func(context.Context, string, string) []string { return finalizeCodexModels([]string{"gpt-6-astra"}) }
	entitled, _ := GetCodexModelIDs(ctx, "entitled-token", "")
	for i, m := range entitled {
		if m == "gpt-6-astra" {
			if i+1 >= len(entitled) || entitled[i+1] != "gpt-6-astra-900k" {
				t.Fatalf("live Astra keeps its 900k twin: %v", entitled)
			}
			return
		}
	}
	t.Fatalf("live Astra expected: %v", entitled)
}

func TestCodexOfflineSourcesConfigDefaultAndCacheLeadTheDefaults(t *testing.T) {
	ctx := modelsTestHome(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("# comment\nmodel = 'gpt-5.4-mini'\n[profiles.x]\nmodel = \"ignored\"\n"), 0o600)
	cache, _ := json.Marshal(map[string]any{"models": []any{
		map[string]any{"slug": "gpt-5.5", "priority": 5},
		map[string]any{"slug": "gpt-5.4", "priority": 1},
		map[string]any{"slug": "  ", "priority": 0},
		map[string]any{"slug": "gpt-hidden", "visibility": "Hide"},
		map[string]any{"slug": "gpt-5.5", "priority": 9},
	}})
	os.WriteFile(filepath.Join(codexHome, "models_cache.json"), cache, 0o600)
	ids, fallback := GetCodexModelIDs(ctx, "", "")
	if !fallback || ids[0] != "gpt-5.4-mini" || ids[1] != "gpt-5.4" || ids[2] != "gpt-5.4-900k" || ids[3] != "gpt-5.5" || containsString(ids, "gpt-hidden") {
		t.Fatalf("config default, then ranked cache, then defaults: %v", ids)
	}
	if readCodexDefaultModel(t.TempDir()) != "" || tomlTopLevelString("model = \"unterminated\n", "model") != "" {
		t.Fatal("missing or malformed config yields no default")
	}
	if got := codexCuratedModels(); got[0] != DefaultCodexModels[0] || !containsString(got, "gpt-5.6-sol-900k") {
		t.Fatalf("curated row is the finalized defaults: %v", got)
	}
}
