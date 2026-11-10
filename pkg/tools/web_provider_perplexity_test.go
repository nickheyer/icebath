package tools

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func perplexityP() *perplexityProvider { return webProvider(nil, "perplexity").(*perplexityProvider) }
func openAINativeP() *openAINativeProvider {
	return webProvider(nil, "openai-native").(*openAINativeProvider)
}

func assertIcebathIdentityHeaders(t *testing.T, h map[string][]string) {
	t.Helper()
	get := func(k string) string {
		if v := h[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if get("Http-Referer") != "https://hermes-agent.nousresearch.com" || get("X-Title") != "Icebath Agent" || get("X-Pplx-Integration") != "icebath-agent" || !strings.HasPrefix(get("User-Agent"), "IcebathAgent/") {
		t.Fatalf("%v", h)
	}
}

func TestPerplexitySearchDispatchMapsSearchAPIShape(t *testing.T) {
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[{"title":"Bloom filter","url":"https://en.wikipedia.org/wiki/Bloom_filter","snippet":"space-efficient probabilistic structure","date":"2004-04-17"}],"id":"abc"}`)
	})
	env := httpxProviderEnv(t, "PERPLEXITY_API_KEY=pplx-test\nPERPLEXITY_BASE_URL="+srv.URL+"/\n", map[string]any{"web.search_backend": "perplexity"})
	decoded, err := pyjson.Loads(WebSearchTool(context.Background(), env, "bloom filter", 3))
	if err != nil {
		t.Fatal(err)
	}
	out := decoded.(*pyjson.Object)
	c := calls()[0]
	if c.path != "/search" || c.header.Get("Authorization") != "Bearer pplx-test" {
		t.Fatalf("%+v", c)
	}
	assertIcebathIdentityHeaders(t, c.header)
	n, _ := c.body["max_results"].(float64)
	if c.body["query"] != "bloom filter" || n < 1 || n > 20 || c.body["search_context_size"] != "low" || c.body["search_type"] != nil {
		t.Fatalf("%v", c.body)
	}
	web := out.Vals["data"].(*pyjson.Object).Vals["web"].([]any)
	want := pyjson.ObjectOf("title", "Bloom filter", "url", "https://en.wikipedia.org/wiki/Bloom_filter", "description", "space-efficient probabilistic structure", "position", int64(1))
	if out.Vals["success"] != true || !pyjson.Equal(web[0], want) {
		t.Fatalf("%v", out)
	}
	if perplexityDefaultBaseURL != "https://api.perplexity.ai" {
		t.Fatal(perplexityDefaultBaseURL)
	}
}

func TestPerplexityExtractSnippetsPerURLAndMissingKey(t *testing.T) {
	urls := []string{"https://tokio.rs/tokio/tutorial", "https://docs.rs/smol", "https://example.com/"}
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[{"url":"`+urls[0]+`","text":"Tokio is an asynchronous runtime … for Rust.","tokens_count":12},{"url":"`+urls[1]+`","error":"Page not found or unavailable."},"junk"]}`)
	})
	env := httpxProviderEnv(t, "PERPLEXITY_API_KEY=pplx-test\nPERPLEXITY_BASE_URL="+srv.URL+"\n", nil)
	docs, _ := perplexityP().Extract(context.Background(), env, urls, "")
	c := calls()[0]
	if c.path != "/sdk/content/snippets" || c.body["query"] != "tokio tutorial smol example.com" || c.body["max_tokens_per_page"].(float64) > c.body["max_tokens"].(float64) {
		t.Fatalf("%+v", c)
	}
	assertIcebathIdentityHeaders(t, c.header)
	if len(docs) != 3 || !strings.Contains(docs[0].Content, "Tokio is an asynchronous runtime") || docs[0].Error != "" || docs[1].Error != "Page not found or unavailable." || docs[2].Error != "no content returned" || docs[2].Metadata["sourceURL"] != urls[2] {
		t.Fatalf("%+v %+v %+v", docs[0], docs[1], docs[2])
	}
	bare := httpxProviderEnv(t, "", nil)
	p := perplexityP()
	if p.Available(bare) {
		t.Fatal("available without key")
	}
	if res, _ := p.Search(context.Background(), bare, "x", 5); res.Success || !strings.Contains(res.Error, "PERPLEXITY_API_KEY") {
		t.Fatalf("%+v", res)
	}
	if pages, _ := p.Extract(context.Background(), bare, []string{"https://example.com"}, ""); !strings.Contains(pages[0].Error, "PERPLEXITY_API_KEY") {
		t.Fatalf("%+v", pages[0])
	}
	if len(calls()) != 1 {
		t.Fatal("missing key still posted")
	}
}

func TestPerplexityQueryForURLs(t *testing.T) {
	for _, tc := range []struct {
		urls []string
		want string
	}{
		{[]string{"https://a.example/Bloom-Filter/2024/bloom_filter"}, "bloom filter"},
		{[]string{"https://a.example/"}, "a.example"},
		{[]string{"https://a.example/x;params?q=1"}, "x"},
		{[]string{"https://a.example/123"}, "https://a.example/123"},
	} {
		if got, err := perplexityQueryForURLs(tc.urls); err != nil || got != tc.want {
			t.Fatalf("%v: %q %v", tc.urls, got, err)
		}
	}
	if _, err := perplexityQueryForURLs([]string{"http://[::1/x"}); err == nil || err.Error() != "Invalid IPv6 URL" {
		t.Fatalf("%v", err)
	}
}

func TestPerplexityErrorsAndManagedRoute(t *testing.T) {
	srv, _ := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid key"}`))
	})
	env := httpxProviderEnv(t, "PERPLEXITY_API_KEY=bad\nPERPLEXITY_BASE_URL="+srv.URL+"\n", nil)
	if res, _ := perplexityP().Search(context.Background(), env, "x", 5); res.Error != `{"error":"invalid key"}` {
		t.Fatalf("%+v", res)
	}
	managed := httpxProviderEnv(t, "", map[string]any{"web.search_backend": "nous"})
	if managedWebSearch(managed) {
		res, _ := perplexityP().Search(context.Background(), managed, "x", 5)
		if res.Error != SelectionError("web", NousManagedProvider, perplexityNoIdentityFailure) {
			t.Fatalf("%+v", res)
		}
	}
	if _, err := normalizePerplexitySnippets(decodeAny(`{"results":[{"url":["x"]}]}`), nil); err == nil || err.Error() != "unhashable type: 'list'" {
		t.Fatalf("%v", err)
	}
}

func TestOpenAINativeProvider(t *testing.T) {
	env := httpxProviderEnv(t, "", nil)
	if openAINativeP().Available(env) {
		t.Fatal("available without auth store")
	}
	write := func(body string) {
		if err := os.WriteFile(home.AuthPath(env.Home), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Dir(home.AuthPath(env.Home)), 0o700)
	for body, want := range map[string]bool{
		`{"providers":{"openai-codex":{"tokens":{"access_token":"tok"}}}}`:            true,
		"\ufeff" + `{"providers":{"openai-codex":{"tokens":{"access_token":"tok"}}}}`: true,
		`{"providers":{"openai-codex":{"tokens":{"access_token":"  "}}}}`:             false,
		`{"credential_pool":{"openai-codex":[{"label":"x"},{"access_token":"t"}]}}`:   true,
		`{"credential_pool":{"openai-codex":{"access_token":"t"}}}`:                   false,
		`{"providers":{"openai-codex":{"tokens":{"access_token":5}}}}`:                true,
		`not json`: false,
	} {
		write(body)
		if got := openAINativeP().Available(env); got != want {
			t.Fatalf("%s: %v", body, got)
		}
	}
	if res, _ := openAINativeP().Search(context.Background(), env, "q", 5); res.Success || res.Error != openAINativeUnsupportedMsg {
		t.Fatalf("%+v", res)
	}
	if openAINativeP().SupportsExtract() || openAINativeP().KeylessAvailable(env) || openAINativeP().SetupSchema()["badge"] != "native" {
		t.Fatal("openai-native capabilities")
	}
}
