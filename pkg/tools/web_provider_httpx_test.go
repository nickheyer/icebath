package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Web env with every httpx vendor variable cleared from the process
func httpxProviderEnv(t *testing.T, dotenv string, settings map[string]any) *Env {
	t.Helper()
	for _, k := range []string{"TAVILY_API_KEY", "TAVILY_BASE_URL", "BRAVE_SEARCH_API_KEY", "SEARXNG_URL", "KEENABLE_API_KEY",
		"FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "PERPLEXITY_API_KEY", "XAI_API_KEY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return sdkProviderEnv(t, dotenv, settings)
}

type recordedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   map[string]any
	raw    string
}

// Fake server recording each request with its query and answering through the handler
func recordingServer(t *testing.T, handler func(r recordedRequest, w http.ResponseWriter)) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		rec := recordedRequest{method: req.Method, path: req.URL.Path, query: req.URL.RawQuery, header: req.Header.Clone(), raw: string(raw)}
		json.Unmarshal(raw, &rec.body)
		mu.Lock()
		got = append(got, rec)
		mu.Unlock()
		handler(rec, w)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), got...)
	}
}

func setVar(t *testing.T, p *string, v string) {
	prev := *p
	*p = v
	t.Cleanup(func() { *p = prev })
}

func tavilyP() *tavilyProvider     { return webProvider(nil, "tavily").(*tavilyProvider) }
func braveP() *braveFreeProvider   { return webProvider(nil, "brave-free").(*braveFreeProvider) }
func searxngP() *searxngProvider   { return webProvider(nil, "searxng").(*searxngProvider) }
func keenableP() *keenableProvider { return webProvider(nil, "keenable").(*keenableProvider) }
func decodeAny(s string) any       { v, _ := pyjson.Loads(s); return v }
func mustJSONMap(s string) map[string]any {
	m := map[string]any{}
	json.Unmarshal([]byte(s), &m)
	return m
}

func TestTavilyRequest(t *testing.T) {
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		if r.body["query"] == "limited" {
			w.WriteHeader(429)
			io.WriteString(w, "Rate limit hit. Sign up for a free API key at https://app.tavily.com")
			return
		}
		writeVendorJSON(w, 200, `{"results":[]}`)
	})
	t.Run("keyless when no api key", func(t *testing.T) {
		env := httpxProviderEnv(t, "TAVILY_BASE_URL="+srv.URL+"\n", nil)
		if _, err := tavilyRequest(context.Background(), env, "search", pyObject("query", "test"), nil); err != nil {
			t.Fatal(err)
		}
		c := calls()[len(calls())-1]
		if c.header.Get("X-Client-Name") != "icebath-agent" || c.header.Get("X-Tavily-Access-Mode") != "keyless" || c.header.Get("Authorization") != "" {
			t.Fatalf("%v", c.header)
		}
		if _, ok := c.body["api_key"]; ok || c.body["query"] != "test" || c.path != "/search" {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("keyed uses bearer not body", func(t *testing.T) {
		env := httpxProviderEnv(t, "TAVILY_BASE_URL="+srv.URL+"\nTAVILY_API_KEY=tvly-test-key\n", nil)
		if _, err := tavilyRequest(context.Background(), env, "/search", pyObject("query", "hello"), nil); err != nil {
			t.Fatal(err)
		}
		c := calls()[len(calls())-1]
		if c.header.Get("Authorization") != "Bearer tvly-test-key" || c.header.Get("X-Tavily-Access-Mode") != "" || c.header.Get("X-Client-Name") != "icebath-agent" {
			t.Fatalf("%v", c.header)
		}
		if _, ok := c.body["api_key"]; ok || c.body["query"] != "hello" || c.raw != `{"query":"hello"}` {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("http error surfaces response body", func(t *testing.T) {
		env := httpxProviderEnv(t, "TAVILY_BASE_URL="+srv.URL+"\n", nil)
		_, err := tavilyRequest(context.Background(), env, "search", pyObject("query", "limited"), nil)
		var ve *verbatimError
		if !errors.As(err, &ve) || !strings.Contains(ve.msg, "Rate limit hit") {
			t.Fatalf("%v", err)
		}
		resp, _ := tavilyP().Search(context.Background(), env, "limited", 5)
		if resp.Success || resp.Error != "Rate limit hit. Sign up for a free API key at https://app.tavily.com" {
			t.Fatalf("%+v", resp)
		}
	})
}

func TestNormalizeTavilySearchResults(t *testing.T) {
	resp, err := normalizeTavilySearchResults(decodeAny(`{"results":[{"title":"Python Docs","url":"https://docs.python.org","content":"Official docs","score":0.9},{"title":"Tutorial","url":"https://example.com","content":"A tutorial","score":0.8}]}`))
	if err != nil || !resp.Success || len(resp.Data.Web) != 2 {
		t.Fatalf("%+v %v", resp, err)
	}
	if w := resp.Data.Web[0]; w.Title != "Python Docs" || w.URL != "https://docs.python.org" || w.Description != "Official docs" || w.Position != 1 || resp.Data.Web[1].Position != 2 {
		t.Fatalf("%+v", resp.Data.Web)
	}
	resp, _ = normalizeTavilySearchResults(decodeAny(`{"results":[{}]}`))
	if w := resp.Data.Web[0]; w.Title != "" || w.URL != "" || w.Description != "" {
		t.Fatalf("%+v", w)
	}
	if _, err := normalizeTavilySearchResults(decodeAny(`[1]`)); err == nil || err.Error() != "'list' object has no attribute 'get'" {
		t.Fatalf("%v", err)
	}
}

func TestNormalizeTavilyDocuments(t *testing.T) {
	docs, err := normalizeTavilyDocuments(decodeAny(`{"results":[{"url":"https://example.com","title":"Example","raw_content":"Full page content here"}]}`))
	if err != nil || len(docs) != 1 {
		t.Fatalf("%v %v", docs, err)
	}
	if d := docs[0]; d.URL != "https://example.com" || d.Title != "Example" || d.Content != "Full page content here" || d.RawContent != d.Content || d.Metadata["sourceURL"] != "https://example.com" {
		t.Fatalf("%+v", d)
	}
	docs, _ = normalizeTavilyDocuments(decodeAny(`{"results":[{"content":"data"}]}`))
	if docs[0].URL != "" || docs[0].Content != "data" {
		t.Fatalf("missing url attributed: %+v", docs[0])
	}
	docs, _ = normalizeTavilyDocuments(decodeAny(`{"results":[],"failed_results":[{"url":"https://a.example","error":"blocked"},{"url":"https://b.example"}],"failed_urls":["https://c.example"]}`))
	if len(docs) != 3 || docs[0].Error != "blocked" || docs[1].Error != "extraction failed" || docs[2].URL != "https://c.example" || docs[2].Metadata["sourceURL"] != "https://c.example" {
		t.Fatalf("%+v", docs)
	}
}

func TestTavilyAvailability(t *testing.T) {
	env := httpxProviderEnv(t, "", map[string]any{"web.keyless_fallback": false})
	if tavilyP().Available(env) || isWebBackendAvailable(env, "tavily") {
		t.Fatal("tavily available without key")
	}
	env = httpxProviderEnv(t, "", map[string]any{"web.backend": "tavily"})
	if !isWebBackendAvailable(env, "tavily") {
		t.Fatal("configured tavily unavailable")
	}
	if !CheckWebAPIKey(env) {
		t.Fatal("configured tavily without key failed the gate")
	}
	for _, tc := range []struct{ dotenv, want string }{
		{"TAVILY_API_KEY=tvly-test\n", "tavily"},
		{"TAVILY_API_KEY=tvly-test\nFIRECRAWL_API_KEY=fc-test\n", "tavily"},
		{"TAVILY_API_KEY=tvly-test\nEXA_API_KEY=exa-test\n", "tavily"},
		{"KEENABLE_API_KEY=kn-test\n", "keenable"},
	} {
		if got := getWebBackend(httpxProviderEnv(t, tc.dotenv, nil)); got != tc.want {
			t.Fatalf("%q: %s", tc.dotenv, got)
		}
	}
}

func TestWebSearchAndExtractTavily(t *testing.T) {
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		if r.path == "/extract" {
			writeVendorJSON(w, 200, `{"results":[{"url":"https://a.example","title":"A","raw_content":"page a"}],"failed_results":[{"url":"https://b.example","error":"timeout"}]}`)
			return
		}
		writeVendorJSON(w, 200, `{"results":[{"title":"Result","url":"https://r.com","content":"desc","score":0.9}]}`)
	})
	env := httpxProviderEnv(t, "TAVILY_BASE_URL="+srv.URL+"\nTAVILY_API_KEY=tvly-test\n", map[string]any{"web.backend": "tavily"})
	out := mustJSONMap(WebSearchTool(context.Background(), env, "test query", 3))
	web := out["data"].(map[string]any)["web"].([]any)
	if out["success"] != true || len(web) != 1 || web[0].(map[string]any)["title"] != "Result" {
		t.Fatalf("%v", out)
	}
	if c := calls()[0]; c.body["max_results"] == nil || c.body["max_results"].(float64) > 20 || c.body["include_raw_content"] != false || c.body["include_images"] != false {
		t.Fatalf("%+v", c.body)
	}
	pages, _ := tavilyP().Extract(context.Background(), env, []string{"https://a.example", "https://b.example"}, "")
	if len(pages) != 2 || pages[0].Content != "page a" || pages[1].Error != "timeout" {
		t.Fatalf("%+v", pages)
	}
	keyless := httpxProviderEnv(t, "TAVILY_BASE_URL="+srv.URL+"\n", map[string]any{"web.backend": "tavily"})
	out = mustJSONMap(WebSearchTool(context.Background(), keyless, "keyless query", 5))
	c := calls()[len(calls())-1]
	if out["success"] != true || c.header.Get("X-Tavily-Access-Mode") != "keyless" || c.header.Get("Authorization") != "" || c.path != "/search" {
		t.Fatalf("%v %v", out, c.header)
	}
	noKeyless := httpxProviderEnv(t, "", map[string]any{"web.keyless_fallback": false})
	resp, _ := tavilyP().Search(context.Background(), noKeyless, "q", 5)
	if resp.Success || resp.Error != tavilyMissingKeyError("search") {
		t.Fatalf("%+v", resp)
	}
	pages, _ = tavilyP().Extract(context.Background(), noKeyless, []string{"https://a.example"}, "")
	if pages[0].Error != tavilyMissingKeyError("extract") {
		t.Fatalf("%+v", pages[0])
	}
}

func TestBraveFreeProvider(t *testing.T) {
	answer := `{"web":{"results":[{"title":"One","url":"https://one.example","description":"first"},{"title":null,"url":"https://two.example","description":"second"},{"title":"Three","url":"https://three.example"}]}}`
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		if strings.Contains(r.query, "q=empty") {
			writeVendorJSON(w, 200, `{"query":{}}`)
			return
		}
		writeVendorJSON(w, 200, answer)
	})
	setVar(t, &braveEndpoint, srv.URL+"/res/v1/web/search")
	env := httpxProviderEnv(t, "BRAVE_SEARCH_API_KEY=BSAkey123\n", nil)
	if !braveP().Available(env) || !isWebBackendAvailable(env, "brave-free") {
		t.Fatal("brave not configured")
	}
	resp, err := braveP().Search(context.Background(), env, "hello world", 2)
	if err != nil || !resp.Success || len(resp.Data.Web) != 2 || resp.Data.Web[1].Title != "None" || resp.Data.Web[0].Description != "first" || resp.Data.Web[1].Position != 2 {
		t.Fatalf("%+v %v", resp, err)
	}
	c := calls()[0]
	if c.header.Get("X-Subscription-Token") != "BSAkey123" || c.header.Get("Accept") != "application/json" || c.query != "q=hello+world&count=2" {
		t.Fatalf("%v %s", c.header, c.query)
	}
	braveP().Search(context.Background(), env, "big", 50)
	braveP().Search(context.Background(), env, "small", 0)
	if q := calls(); !strings.HasSuffix(q[1].query, "count=20") || !strings.HasSuffix(q[2].query, "count=1") {
		t.Fatalf("%v", q)
	}
	resp, _ = braveP().Search(context.Background(), env, "empty", 5)
	if !resp.Success || len(resp.Data.Web) != 0 {
		t.Fatalf("%+v", resp)
	}
	resp, _ = braveP().Search(context.Background(), httpxProviderEnv(t, "", nil), "q", 5)
	if resp.Success || resp.Error != "BRAVE_SEARCH_API_KEY is not set" {
		t.Fatalf("%+v", resp)
	}
	paid := httpxProviderEnv(t, "EXA_API_KEY=exa_test\nBRAVE_SEARCH_API_KEY=BSAkey123\n", nil)
	if got := getWebBackend(paid); got != "exa" {
		t.Fatalf("brave overrode exa: %s", got)
	}
	configured := httpxProviderEnv(t, "BRAVE_SEARCH_API_KEY=BSAkey123\n", map[string]any{"web.backend": "brave-free"})
	if !CheckWebAPIKey(configured) {
		t.Fatal("brave failed the gate")
	}
	out := mustJSONMap(WebExtractTool(context.Background(), configured, []any{"https://example.com"}, "", nil))
	if msg := strings.ToLower(asStr(out["error"])); out["success"] != false || !strings.Contains(msg, "search-only") || !strings.Contains(msg, "brave") {
		t.Fatalf("%v", out)
	}
}

func TestBraveFreeHTTPFailures(t *testing.T) {
	srv, _ := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		switch {
		case strings.Contains(r.query, "q=forbidden"):
			w.WriteHeader(403)
		case strings.Contains(r.query, "q=redirect"):
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(302)
		case strings.Contains(r.query, "q=garbage"):
			io.WriteString(w, "not json")
		default:
			writeVendorJSON(w, 200, `{"web":{"results":{"a":1}}}`)
		}
	})
	setVar(t, &braveEndpoint, srv.URL)
	env := httpxProviderEnv(t, "BRAVE_SEARCH_API_KEY=k\n", nil)
	for q, want := range map[string]string{
		"forbidden": "Brave Search returned HTTP 403",
		"redirect":  "Brave Search returned HTTP 302",
		"garbage":   "Could not parse Brave Search response as JSON",
	} {
		if resp, _ := braveP().Search(context.Background(), env, q, 5); resp.Success || resp.Error != want {
			t.Fatalf("%s: %+v", q, resp)
		}
	}
	if _, err := braveP().Search(context.Background(), env, "dict", 5); err == nil || err.Error() != "slice(None, 5, None)" {
		t.Fatalf("%v", err)
	}
	setVar(t, &braveEndpoint, "localhost:1/search")
	if resp, _ := braveP().Search(context.Background(), env, "q", 5); resp.Error != "Could not reach Brave Search: Request URL has an unsupported protocol 'localhost://'." {
		t.Fatalf("%+v", resp)
	}
}

func TestSearXNGProvider(t *testing.T) {
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[{"title":"Low","url":"https://low.example","content":"low","score":0.2},{"title":"High","url":"https://high.example","content":"high","score":"2.5"},{"title":"Mid","url":"https://mid.example","content":"mid","score":1},{"title":"NoScore","url":"https://none.example","content":"none"},{"title":"Mid2","url":"https://mid2.example","content":"mid2","score":1.0}]}`)
	})
	env := httpxProviderEnv(t, "SEARXNG_URL="+srv.URL+"//\n", nil)
	if !searxngP().Available(env) || !isWebBackendAvailable(env, "searxng") {
		t.Fatal("searxng not configured")
	}
	resp, err := searxngP().Search(context.Background(), env, "q x", 4)
	if err != nil || !resp.Success {
		t.Fatalf("%+v %v", resp, err)
	}
	var titles []string
	for _, w := range resp.Data.Web {
		titles = append(titles, w.Title)
	}
	if strings.Join(titles, ",") != "High,Mid,Mid2,Low" || resp.Data.Web[0].Description != "high" || resp.Data.Web[3].Position != 4 {
		t.Fatalf("%v", titles)
	}
	if c := calls()[0]; c.path != "/search" || c.query != "q=q+x&format=json&pageno=1" || c.header.Get("Accept") != "application/json" {
		t.Fatalf("%+v", c)
	}
	resp, _ = searxngP().Search(context.Background(), httpxProviderEnv(t, "", nil), "q", 5)
	if resp.Success || resp.Error != "SEARXNG_URL is not set" {
		t.Fatalf("%+v", resp)
	}
	refused := httpxProviderEnv(t, "SEARXNG_URL=http://127.0.0.1:1\n", nil)
	resp, _ = searxngP().Search(context.Background(), refused, "q", 5)
	if !strings.HasPrefix(resp.Error, "Could not reach SearXNG at http://127.0.0.1:1: [") {
		t.Fatalf("%+v", resp)
	}
	if got := getWebBackend(httpxProviderEnv(t, "SEARXNG_URL=http://localhost:8080\n", map[string]any{"web.backend": "searxng"})); got != "searxng" {
		t.Fatal(got)
	}
	if got := getWebBackend(httpxProviderEnv(t, "EXA_API_KEY=exa_test_key\nSEARXNG_URL=http://localhost:8080\n", nil)); got != "exa" {
		t.Fatal(got)
	}
	if got := getWebBackend(httpxProviderEnv(t, "SEARXNG_URL=http://localhost:8080\n", map[string]any{"web.keyless_fallback": false})); got != "searxng" {
		t.Fatal(got)
	}
	if !CheckWebAPIKey(httpxProviderEnv(t, "SEARXNG_URL=http://localhost:8080\n", map[string]any{"web.backend": "searxng"})) {
		t.Fatal("searxng failed the gate")
	}
	if CheckWebAPIKey(httpxProviderEnv(t, "", map[string]any{"web.keyless_fallback": false})) {
		t.Fatal("no credentials passed the gate")
	}
	out := mustJSONMap(WebExtractTool(context.Background(), httpxProviderEnv(t, "SEARXNG_URL=http://localhost:8080\n", map[string]any{"web.backend": "searxng"}), []any{"https://example.com"}, "", nil))
	if msg := strings.ToLower(asStr(out["error"])); out["success"] != false || !strings.Contains(msg, "searxng") {
		t.Fatalf("%v", out)
	}
}

func TestSearXNGScoreErrorsRaise(t *testing.T) {
	srv, _ := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[{"title":"a","score":"high"}]}`)
	})
	env := httpxProviderEnv(t, "SEARXNG_URL="+srv.URL+"\n", nil)
	if _, err := searxngP().Search(context.Background(), env, "q", 5); err == nil || err.Error() != "could not convert string to float: 'high'" {
		t.Fatalf("%v", err)
	}
}

func TestKeenableKeyedSearchAndExtract(t *testing.T) {
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		switch {
		case r.path == "/v1/search":
			writeVendorJSON(w, 200, `{"results":[{"url":"https://k.example","title":"K","snippet":"","description":"desc"}]}`)
		case strings.Contains(r.query, "missing"):
			writeVendorJSON(w, 404, `{"message":"The page does not exist on the target server"}`)
		case strings.Contains(r.query, "broken"):
			w.WriteHeader(500)
			io.WriteString(w, "upstream exploded")
		default:
			writeVendorJSON(w, 200, `{"url":"https://www.final.example/moved","title":"Moved","content":"body"}`)
		}
	})
	setVar(t, &keenableAPIURL, srv.URL)
	env := httpxProviderEnv(t, "KEENABLE_API_KEY=kn-from-dotenv\n", nil)
	if !keenableP().Available(env) {
		t.Fatal("keenable key from .env ignored")
	}
	resp, _ := keenableP().Search(context.Background(), env, "q", 50)
	if !resp.Success || resp.Data.Web[0].Description != "desc" || resp.Data.Web[0].URL != "https://k.example" {
		t.Fatalf("%+v", resp)
	}
	c := calls()[0]
	if c.header.Get("Authorization") != "Bearer kn-from-dotenv" || c.header.Get("X-Keenable-Title") != "icebath-agent" || c.raw != `{"query": "q", "max_results": 20}` {
		t.Fatalf("%v %s", c.header, c.raw)
	}
	pages, _ := keenableP().Extract(context.Background(), env, []string{"https://old.example/a", "https://missing.example", "https://broken.example"}, "")
	if len(pages) != 3 || pages[0].URL != "https://www.final.example/moved" || pages[0].Metadata["sourceURL"] != "https://old.example/a" {
		t.Fatalf("%+v", pages[0])
	}
	if !isOriginHTTPError(pages[1].Error) || pages[2].Error != "Keenable extract failed: upstream exploded" {
		t.Fatalf("%q %q", pages[1].Error, pages[2].Error)
	}
	srvFail, _ := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		if strings.Contains(r.raw, "garbage") {
			io.WriteString(w, "nope")
			return
		}
		w.WriteHeader(503)
		io.WriteString(w, " busy ")
	})
	setVar(t, &keenableAPIURL, srvFail.URL)
	if resp, _ := keenableP().Search(context.Background(), env, "q", 5); resp.Error != "Keenable search failed: busy" {
		t.Fatalf("%+v", resp)
	}
	if resp, _ := keenableP().Search(context.Background(), env, "garbage", 5); resp.Error != "Keenable search failed: Expecting value: line 1 column 1 (char 0)" {
		t.Fatalf("%+v", resp)
	}
}

func TestKeenableKeylessUsesTheRing(t *testing.T) {
	env := httpxProviderEnv(t, "", map[string]any{"web.provider_tier.keenable": "free"})
	var searched, extracted bool
	swapKeyless(t, "keenable", func(context.Context, string, int) *WebSearchResponse { searched = true; return searchOK(nil) },
		func(_ context.Context, urls []string) []*WebPage { extracted = true; return extractFail(urls, "x") })
	keenableP().Search(context.Background(), env, "q", 3)
	keenableP().Extract(context.Background(), env, []string{"https://a.example"}, "")
	if !searched || !extracted {
		t.Fatalf("keyless ring skipped %v %v", searched, extracted)
	}
}

func TestHttpxProvidersReadKeysFromTheDotenvLayer(t *testing.T) {
	for _, tc := range []struct {
		p   WebProvider
		key string
	}{{tavilyP(), "TAVILY_API_KEY"}, {braveP(), "BRAVE_SEARCH_API_KEY"}, {keenableP(), "KEENABLE_API_KEY"}} {
		if tc.p.Available(httpxProviderEnv(t, "", nil)) {
			t.Fatalf("%s available without key", tc.key)
		}
		if !tc.p.Available(httpxProviderEnv(t, tc.key+"=test-key-from-dotenv\n", nil)) {
			t.Fatalf("%s ignored the .env layer", tc.key)
		}
	}
}

func TestHttpxProvidersHonourInterrupts(t *testing.T) {
	env := httpxProviderEnv(t, "TAVILY_API_KEY=k\nKEENABLE_API_KEY=k\n", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, p := range []WebProvider{tavilyP(), keenableP()} {
		if resp, _ := p.Search(ctx, env, "q", 5); resp.Error != "Interrupted" {
			t.Fatalf("%s %+v", p.Name(), resp)
		}
		if pages, _ := p.Extract(ctx, env, []string{"https://a.example"}, ""); pages[0].Error != "Interrupted" {
			t.Fatalf("%s %+v", p.Name(), pages)
		}
	}
}

const (
	gateRedirector = "https://redirector.example/go"
	gateBlocked    = "https://blocked.example/secret"
)

// Keyed Keenable whose fetch reports the target url as the final page, counting fetches
func redirectingKeenable(t *testing.T, target string) (*Env, *int) {
	fetches := 0
	var mu sync.Mutex
	srv, _ := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		mu.Lock()
		fetches++
		mu.Unlock()
		body, _ := json.Marshal(map[string]any{"url": target, "title": "T", "content": "TARGET PAGE"})
		writeVendorJSON(w, 200, string(body))
	})
	setVar(t, &keenableAPIURL, srv.URL)
	prev := extractWebsiteAccess
	extractWebsiteAccess = func(env *Env, u, path string) (*WebsiteBlock, error) {
		if strings.Contains(u, "blocked.example") {
			return &WebsiteBlock{Host: "blocked.example", Rule: "blocked.example", Source: "config", Message: "Blocked: blocked.example is blocked by website policy"}, nil
		}
		return nil, nil
	}
	prevSafe := extractURLSafe
	extractURLSafe = func(env *Env, u string) bool { return !strings.Contains(u, "internal.example") }
	t.Cleanup(func() { extractWebsiteAccess, extractURLSafe = prev, prevSafe })
	return httpxProviderEnv(t, "KEENABLE_API_KEY=test-key\n", map[string]any{"web.provider_tier.keenable": "paid"}), &fetches
}

func gateExtract(t *testing.T, env *Env) *WebPage {
	pages, err := extractSafeURLs(webCallContext{context.Background(), env}, keenableP(), []string{gateRedirector}, "")
	if err != nil || len(pages) != 1 {
		t.Fatalf("%v %v", pages, err)
	}
	return pages[0]
}

func TestKeenableRedirectIntoABlockedHostIsRefusedAndNeverCached(t *testing.T) {
	env, fetches := redirectingKeenable(t, gateBlocked)
	for range 3 {
		p := gateExtract(t, env)
		if p.Content != "" || strings.Contains(p.RawContent, "TARGET PAGE") || p.URL != gateBlocked || !strings.HasPrefix(p.Error, "Blocked") {
			t.Fatalf("%+v", p)
		}
		if p.BlockedByPolicy["host"] != "blocked.example" || p.BlockedByPolicy["rule"] != "blocked.example" || p.BlockedByPolicy["source"] != "config" {
			t.Fatalf("%v", p.BlockedByPolicy)
		}
	}
	if *fetches != 3 {
		t.Fatalf("fetches %d", *fetches)
	}
}

func TestKeenableAllowedRedirectIsServedFromCacheWithoutARefetchLoop(t *testing.T) {
	env, fetches := redirectingKeenable(t, "https://allowed.example/page")
	if p := gateExtract(t, env); p.Content != "TARGET PAGE" {
		t.Fatalf("%+v", p)
	}
	for range 2 {
		if p := gateExtract(t, env); !p.Cached || p.Content != "TARGET PAGE" {
			t.Fatalf("%+v", p)
		}
	}
	if *fetches != 1 {
		t.Fatalf("fetches %d", *fetches)
	}
}

func TestKeenableRedirectToAPrivateAddressIsRefused(t *testing.T) {
	env, _ := redirectingKeenable(t, "http://internal.example/admin")
	if p := gateExtract(t, env); p.Content != "" || !strings.Contains(p.Error, "private or internal") || p.BlockedByPolicy != nil {
		t.Fatalf("%+v", p)
	}
}

func TestPolicyCheckThatRaisesOnACachedFinalURLIsAMiss(t *testing.T) {
	env, fetches := redirectingKeenable(t, "https://allowed.example/page")
	gateExtract(t, env)
	extractWebsiteAccess = func(env *Env, u, path string) (*WebsiteBlock, error) {
		if strings.Contains(u, "allowed.example") {
			return nil, errors.New("policy unreadable")
		}
		return nil, nil
	}
	p := gateExtract(t, env)
	if p.Cached || *fetches != 2 || p.Content != "" || !strings.Contains(p.Error, "policy check failed") {
		t.Fatalf("%+v fetches %d", p, *fetches)
	}
}

func TestKeylessModeKeenableIsNotRescueEligible(t *testing.T) {
	if rescueEligible(httpxProviderEnv(t, "", map[string]any{"web.backend": "keenable"}), keenableP()) {
		t.Fatal("keyless keenable rescued")
	}
	if !rescueEligible(httpxProviderEnv(t, "KEENABLE_API_KEY=kn-real\n", map[string]any{"web.backend": "keenable"}), keenableP()) {
		t.Fatal("keyed keenable not rescued")
	}
}
