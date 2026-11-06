package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A test env with every web credential cleared like _clear_web_env
func clearedWebEnv(t *testing.T, settings map[string]any) *Env {
	t.Helper()
	env := httpxProviderEnv(t, "", settings)
	for _, k := range []string{"BRAVE_SEARCH_API_KEY", "SEARXNG_URL", "KEENABLE_API_KEY", "TAVILY_API_KEY", "TAVILY_BASE_URL", "EXA_API_KEY", "PARALLEL_API_KEY",
		"PARALLEL_SEARCH_MODE", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "FIRECRAWL_GATEWAY_URL", "TOOL_GATEWAY_DOMAIN", "TOOL_GATEWAY_USER_TOKEN", "XAI_API_KEY"} {
		t.Setenv(k, "")
	}
	return env
}

// Each keyed provider is unavailable without its credential and available once it is set
func TestWebSearchPluginIsAvailable(t *testing.T) {
	cases := []struct{ name, key, value string }{
		{"brave-free", "BRAVE_SEARCH_API_KEY", "real"},
		{"searxng", "SEARXNG_URL", "http://localhost:8080"},
		{"keenable", "KEENABLE_API_KEY", "real"},
		{"tavily", "TAVILY_API_KEY", "real"},
		{"exa", "EXA_API_KEY", "real"},
		{"parallel", "PARALLEL_API_KEY", "real"},
		{"firecrawl", "FIRECRAWL_API_KEY", "real"},
		{"firecrawl", "FIRECRAWL_API_URL", "http://localhost:3002"},
		{"xai", "XAI_API_KEY", "real"},
	}
	for _, c := range cases {
		env := clearedWebEnv(t, nil)
		p := webProvider(env, c.name)
		if p == nil {
			t.Fatalf("%s not registered", c.name)
		}
		if p.Available(env) {
			t.Errorf("%s available without %s", c.name, c.key)
		}
		t.Setenv(c.key, c.value)
		if !p.Available(env) {
			t.Errorf("%s unavailable with %s", c.name, c.key)
		}
	}
}

func TestWebSearchPluginFirecrawlExplicitConfigAllowsKeylessCloud(t *testing.T) {
	p := webProvider(nil, "firecrawl")
	if p.Available(clearedWebEnv(t, nil)) {
		t.Fatal("firecrawl available without credentials or config")
	}
	if !p.Available(clearedWebEnv(t, map[string]any{"web.backend": "firecrawl"})) {
		t.Fatal("explicit firecrawl backend not available")
	}
}

func TestWebSearchRegistryExplicitConfigWinsEvenWhenUnavailable(t *testing.T) {
	env := clearedWebEnv(t, nil)
	p := resolveWebProvider(env, "brave-free", "search")
	if p == nil || p.Name() != "brave-free" || p.Available(env) {
		t.Fatalf("%v", p)
	}
}

func TestWebSearchRegistryUnknownNameFallsBackToAvailableProvider(t *testing.T) {
	env := clearedWebEnv(t, nil)
	t.Setenv("EXA_API_KEY", "real")
	p := resolveWebProvider(env, "not-a-real-provider", "search")
	if p == nil || !p.Available(env) {
		t.Fatalf("%v", p)
	}
}

func TestWebSearchRegistryNoConfigNoCredentials(t *testing.T) {
	env := clearedWebEnv(t, nil)
	if p := resolveWebProvider(env, "", "search"); p != nil && !p.Available(env) && !providerKeylessAvailable(env, p) {
		t.Fatalf("%s resolved with neither credentials nor keyless access", p.Name())
	}
}

// The scrape request carries the 60000 ms server deadline matching the client wait
func TestFirecrawlExtractPassesTimeoutMsToScrape(t *testing.T) {
	var scrapes []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if r.URL.Path == "/v2/scrape" {
			scrapes = append(scrapes, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success": true, "data": {"metadata": {"title": "Test", "sourceURL": "https://example.com"}, "markdown": "content"}}`)
	}))
	defer srv.Close()
	env := clearedWebEnv(t, nil)
	t.Setenv("FIRECRAWL_API_KEY", "test-key")
	t.Setenv("FIRECRAWL_API_URL", srv.URL)
	pages, err := webProvider(env, "firecrawl").Extract(context.Background(), env, []string{"https://example.com"}, "")
	if err != nil || len(scrapes) != 1 {
		t.Fatalf("%v %v", scrapes, err)
	}
	if scrapes[0]["timeout"] != float64(60000) {
		t.Fatalf("Expected timeout=60000, got %v", scrapes[0]["timeout"])
	}
	if len(pages) != 1 || pages[0].URL != "https://example.com" || pages[0].Content != "content" {
		t.Fatalf("%+v", pages)
	}
}
