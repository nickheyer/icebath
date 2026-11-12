package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// Envelope of an MCP tools/call result carrying one text item
func keylessEnvelope(text string) string {
	b, _ := json.Marshal(map[string]any{"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}})
	return string(b)
}

// Local server answering every request with handler, its url swapped into target for the test
func keylessServer(t *testing.T, target *string, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	prev := *target
	*target = srv.URL
	t.Cleanup(func() { *target = prev; srv.Close() })
}

func TestParseMCPBodyCases(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	sse := "event: message\ndata: " + keylessEnvelope("hello") + "\n\n"
	if got, err := parseMCPBody(sse); err != nil || got != "hello" {
		t.Fatalf("sse %q %v", got, err)
	}
	if got, err := parseMCPBody(keylessEnvelope("hi")); err != nil || got != "hi" {
		t.Fatalf("plain %q %v", got, err)
	}
	for body, want := range map[string]string{
		`{"error": {"code": -32000, "message": "rate limit"}}`:                         "rate limit",
		`{"result": {"isError": true, "content": [{"type": "text", "text": "boom"}]}}`: "boom",
		`{"result": {"content": []}}`:                                                  "no text content",
		"<html>nope</html>":                                                            "",
	} {
		_, err := parseMCPBody(body)
		if !isKeylessMCPError(err) || !strings.Contains(err.Error(), want) {
			t.Errorf("parseMCPBody(%q) error %v", body, err)
		}
	}
	for _, term := range []string{"\r", "\r\n"} {
		body := "event: message" + term + "data: " + keylessEnvelope("Title: hello") + term + term
		if got, err := parseMCPBody(body); err != nil || got != "Title: hello" {
			t.Errorf("terminator %q gave %q %v", term, got, err)
		}
	}
}

func TestMCPCallDecodesLikeResponseText(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	title := "光伏发电站组件清洗与性能监测规范"
	keylessServer(t, &exaMCPURL, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "icebath-agent" || r.Header.Get("Accept") != "application/json, text/event-stream" {
			t.Errorf("headers %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message\ndata: {\"result\": {\"content\": [{\"type\": \"text\", \"text\": \"Title: "+title+"\\nURL: https://x.example\"}]}}\n\n")
	})
	if text, err := mcpCall(context.Background(), exaMCPURL, "web_search_exa", map[string]any{"query": title}); err != nil || !strings.Contains(text, title) {
		t.Fatalf("cjk %q %v", text, err)
	}
	gbk, _ := simplifiedchinese.GBK.NewEncoder().String("event: message\ndata: {\"result\": {\"content\": [{\"type\": \"text\", \"text\": \"Title: 你好\"}]}}\n\n")
	keylessServer(t, &exaMCPURL, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=gbk")
		io.WriteString(w, gbk)
	})
	if text, err := mcpCall(context.Background(), exaMCPURL, "web_search_exa", nil); err != nil || text != "Title: 你好" {
		t.Fatalf("gbk %q %v", text, err)
	}
	busy := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(429)
		io.WriteString(w, "请求过多")
	}
	keylessServer(t, &exaMCPURL, busy)
	if _, err := mcpCall(context.Background(), exaMCPURL, "web_search_exa", nil); !isKeylessMCPError(err) || err.Error() != "HTTP 429: 请求过多" {
		t.Fatalf("mcp error %v", err)
	}
	keylessServer(t, &keenableAPIURL, busy)
	if _, err := keenableRequest(context.Background(), http.MethodGet, "/v1/search/public", nil, nil); !isKeylessMCPError(err) || !strings.Contains(err.Error(), "请求过多") {
		t.Fatalf("keenable error %v", err)
	}
	keylessServer(t, &exaMCPURL, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=no-such-codec")
		io.WriteString(w, "x")
	})
	if _, err := mcpCall(context.Background(), exaMCPURL, "web_search_exa", nil); err == nil || isKeylessMCPError(err) || err.Error() != "unknown encoding: no-such-codec" {
		t.Fatalf("lookup error %v", err)
	}
}

func TestKeenableRequestErrorsAndDecoding(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	keylessServer(t, &keenableAPIURL, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Keenable-Title") != "icebath" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(403)
		io.WriteString(w, "anonymous access disabled")
	})
	if _, err := keenableRequest(context.Background(), http.MethodPost, "/v1/search/public", nil, map[string]any{"query": "q"}); !isKeylessMCPError(err) || err.Error() != "HTTP 403: anonymous access disabled" {
		t.Fatalf("status error %v", err)
	}
	body := ""
	keylessServer(t, &keenableAPIURL, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})
	body = `{"results": [{"url": "https://a", "title": NaN, "snippet": Infinity}]}`
	out, err := keenableSearchKeyless(context.Background(), "q", 5)
	if err != nil || !out.Success || out.data().Web[0].Title != "nan" || out.data().Web[0].Description != "inf" {
		t.Fatalf("nan payload %+v %v", out, err)
	}
	for payload, want := range map[string]string{
		"not json":            "Keyless Keenable search failed: Expecting value: line 1 column 1 (char 0).",
		`[1, 2]`:              "Keyless Keenable search failed: 'list' object has no attribute 'get'.",
		`{"results": 5}`:      "Keyless Keenable search failed: 'int' object is not iterable.",
		`{"results": ["x"]}`:  "Keyless Keenable search failed: 'str' object has no attribute 'get'.",
		`{"results": [], "x"`: "Keyless Keenable search failed: Expecting ':' delimiter: line 1 column 20 (char 19).",
	} {
		body = payload
		out, err := keenableSearchKeyless(context.Background(), "q", 5)
		if err != nil || out.Success || out.Error != want {
			t.Errorf("payload %q gave %+v %v", payload, out, err)
		}
	}
	body = `[1]`
	pages, err := keenableExtractKeyless(context.Background(), []string{"https://a"})
	if err != nil || len(pages) != 1 || pages[0].Error != "Keyless Keenable extract failed: 'list' object has no attribute 'get'. Set KEENABLE_API_KEY (https://keenable.ai) for reliable service." {
		t.Fatalf("extract %+v %v", pages, err)
	}
	keenableAPIURL = "http://127.0.0.1:1"
	out, err = keenableSearchKeyless(context.Background(), "q", 5)
	if err != nil || out.Success || !strings.HasPrefix(out.Error, "Keyless Keenable search failed: ") || strings.Contains(out.Error, "request failed") || strings.Contains(out.Error, "KEENABLE_API_KEY") {
		t.Fatalf("transport failure %+v %v", out, err)
	}
}

func TestKeylessFirecrawlErrorKeepsStatusAndReason(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	keylessServer(t, &firecrawlCloudAPIURL, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		io.WriteString(w, "your IP address looks suspicious\n")
	})
	_, err := newKeylessFirecrawlClient(firecrawlCloudAPIURL).search(context.Background(), "q", 1)
	if err == nil || err.Error() != "HTTP 403: your IP address looks suspicious" {
		t.Fatalf("error %v", err)
	}
}

func TestExaTextParsing(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	text := "Title: First\nURL: https://a.example\nPublished: N/A\nHighlights:\nsome highlight\nmore\n\n---\nTitle: Second\nURL: https://b.example\nHighlights:\nother\n"
	hits := parseExaSearchText(text, 5)
	if len(hits) != 2 || hits[0].URL != "https://a.example" || hits[1].URL != "https://b.example" || hits[0].Description != "some highlight more" || hits[0].Position != 1 {
		t.Fatalf("hits %+v", hits)
	}
	var blocks []string
	for i := 0; i < 6; i++ {
		blocks = append(blocks, "Title: T"+string(rune('0'+i))+"\nURL: https://x"+string(rune('0'+i))+".example")
	}
	if got := parseExaSearchText(strings.Join(blocks, "\n---\n"), 2); len(got) != 2 {
		t.Fatalf("limit %d", len(got))
	}
	if got := parseExaSearchText("Title: A\rURL: https://a\x1c\nHighlights:\n\x1chi\x1c", 0); len(got) != 1 || got[0].URL != "https://a" || got[0].Description != "hi" {
		t.Fatalf("splitlines %+v", got)
	}
	if got := parseExaSearchText("Title: none\n---\nTitle: B\nURL: https://b", -1); len(got) != 0 {
		t.Fatalf("negative limit %+v", got)
	}
}

func TestKeylessCalls(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var lastTool string
	var lastArgs map[string]any
	reply := ""
	var calls atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		lastTool, lastArgs = req.Params.Name, req.Params.Arguments
		io.WriteString(w, reply)
	}
	keylessServer(t, &parallelMCPURL, handler)
	keylessServer(t, &exaMCPURL, handler)
	reply = keylessEnvelope(`{"results": [{"url": "https://a", "title": "A", "excerpts": ["x", "y"]}, {"url": "https://b", "title": "B", "excerpts": []}]}`)
	out, err := parallelSearchKeyless(context.Background(), "query", 5)
	if err != nil || !out.Success || out.data().Web[0] != (WebSearchHit{URL: "https://a", Title: "A", Description: "x y", Position: 1}) {
		t.Fatalf("parallel search %+v %v", out, err)
	}
	if _, has := lastArgs["model_name"]; lastTool != "web_search" || has {
		t.Fatalf("parallel call %s %v", lastTool, lastArgs)
	}
	if out, _ := parallelSearchKeyless(context.Background(), "query", -1); !out.Success || len(out.data().Web) != 0 {
		t.Fatalf("negative limit %+v", out)
	}
	if out, _ := parallelSearchKeyless(context.Background(), "query", 0); len(out.data().Web) != 2 {
		t.Fatalf("zero limit %+v", out)
	}
	for payload, want := range map[string]string{
		`{"results": 5}`:                   "Keyless Parallel search returned an unexpected payload: 'int' object is not subscriptable",
		`{"results": {"a": 1}}`:            "Keyless Parallel search returned an unexpected payload: unhashable type: 'slice'",
		`{"results": [{"excerpts": [1]}]}`: "Keyless Parallel search returned an unexpected payload: sequence item 0: expected str instance, int found",
		`{"results": [{"excerpts": 3}]}`:   "Keyless Parallel search returned an unexpected payload: can only join an iterable",
		`{"results": [], "x" 1}`:           "Keyless Parallel search returned an unexpected payload: Expecting ':' delimiter: line 1 column 21 (char 20)",
		`{"results": [{"url": Infinity}]}`: "",
	} {
		reply = keylessEnvelope(payload)
		out, err := parallelSearchKeyless(context.Background(), "q", 5)
		if err != nil || (want == "" && (!out.Success || out.data().Web[0].URL != "inf")) || (want != "" && out.Error != want) {
			t.Errorf("payload %q gave %+v %v", payload, out, err)
		}
	}
	for payload, want := range map[string]string{`[1]`: "'list' object has no attribute 'get'", `{"results": "ab"}`: "'str' object has no attribute 'get'"} {
		reply = keylessEnvelope(payload)
		if _, err := parallelSearchKeyless(context.Background(), "q", 5); err == nil || err.Error() != want {
			t.Errorf("payload %q propagated %v", payload, err)
		}
	}
	reply = `{"error": {"code": -1, "message": "429"}}`
	if out, _ := parallelSearchKeyless(context.Background(), "q", 5); out.Success || !strings.Contains(out.Error, "PARALLEL_API_KEY") {
		t.Fatalf("parallel failure %+v", out)
	}
	reply = keylessEnvelope(`{"results": [{"url": "https://a", "title": "A", "excerpts": ["c"]}]}`)
	pages, err := parallelExtractKeyless(context.Background(), []string{"https://a", "https://gone"})
	if err != nil || pages[0].Content != "c" || pages[1].URL != "https://gone" || pages[1].Error == "" {
		t.Fatalf("parallel extract %+v %v", pages, err)
	}
	reply = `{"error": {"code": -1, "message": "free MCP rate limit"}}`
	if out, _ := exaSearchKeyless(context.Background(), "q", 5); out.Success || !strings.Contains(out.Error, "EXA_API_KEY") {
		t.Fatalf("exa rate limit %+v", out)
	}
	if lastArgs["numResults"] != float64(5) {
		t.Fatalf("exa args %v", lastArgs)
	}
	reply = `{"result": {"content": [{"type": "text", "text": "x"}]}, "error": [1]}`
	if _, err := exaSearchKeyless(context.Background(), "q", 5); err == nil || isKeylessMCPError(err) {
		t.Fatalf("exa non mcp error swallowed %v", err)
	}
	reply = keylessEnvelope("# Page Title\nbody text")
	calls.Store(0)
	pages, err = exaExtractKeyless(context.Background(), []string{"https://a", "https://b"})
	if err != nil || calls.Load() != 2 || pages[0].Title != "Page Title" || !strings.HasPrefix(pages[0].Content, "# Page Title") {
		t.Fatalf("exa extract %+v %v", pages, err)
	}
}

// Env pinning the keyless ring to name, with optional per vendor tiers
func keylessPinnedEnv(t *testing.T, name string, tiers map[string]any) *Env {
	web := map[string]any{}
	if name != "" {
		web["backend"] = name
	}
	if tiers != nil {
		web["provider_tier"] = tiers
	}
	return &Env{Home: t.TempDir(), Config: config.Tree{"web": web}}
}

func keylessOK(vendor string) func(context.Context, string, int) *WebSearchResponse {
	return func(context.Context, string, int) *WebSearchResponse {
		return searchOK([]WebSearchHit{{URL: "https://" + vendor + ".example"}})
	}
}

func keylessThrottled(vendor string) func(context.Context, string, int) *WebSearchResponse {
	return func(context.Context, string, int) *WebSearchResponse {
		return searchFail("Keyless " + vendor + " search failed: free MCP rate limit.")
	}
}

func TestKeylessFailover(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for message, want := range map[string]bool{
		"free MCP rate limit": true, "Client error '403 Forbidden'": true, "HTTP status 401: unauthorized": true, "status code=403": true,
		"HTTP 2403": false, "error 4031": false, "FORBIDDEN": false, "extract failed for 'forbidden kingdom trailer': connection timeout": false,
		"HTTP 400: malformed request": false, "HTTP 402: free credits exhausted": true, "HTTP 503: upstream unavailable": true, "HTTP 404: not found": false,
		"Keyless Firecrawl search failed: HTTP 403: your IP address looks suspicious": true, "HTTP 400: invalid query 'http 503'": false,
		"HTTP 422: query contains 'status=500'": false, "search failed for 'HTTP 503 error codes': timeout": false,
		"error: 500 results max exceeded": false, "Keyless Exa search failed: HTTP 400: invalid query 'http 403'": false, "": false,
		"HTTP 5\u0660\u0663: busy": true, "HTTP \u0665\u0660\u0663: busy": false, "HTTP 503: busy": true, "RATE LİMIT": false,
	} {
		if got := isSearchFailoverEligible(message); got != want {
			t.Errorf("isSearchFailoverEligible(%q) = %v", message, got)
		}
	}
	ctx := context.Background()
	swapKeyless(t, "exa", keylessThrottled("Exa"), nil)
	swapKeyless(t, "parallel", keylessOK("parallel"), nil)
	if out, err := keylessSearchWithFailover(ctx, keylessPinnedEnv(t, "exa", nil), "exa", "q", 3); err != nil || !out.Success || out.data().ServedBy != "parallel" {
		t.Fatalf("rate limit failover %+v %v", out, err)
	}
	swapKeyless(t, "firecrawl", func(context.Context, string, int) *WebSearchResponse {
		return searchFail("Keyless Firecrawl search failed: Client error '403 Forbidden'")
	}, nil)
	swapKeyless(t, "keenable", keylessOK("keenable"), nil)
	if out, _ := keylessSearchWithFailover(ctx, keylessPinnedEnv(t, "firecrawl", nil), "firecrawl", "q", 5); !out.Success || out.data().ServedBy != "keenable" {
		t.Fatalf("forbidden failover %+v", out)
	}
	called := false
	swapKeyless(t, "exa", func(context.Context, string, int) *WebSearchResponse {
		return searchFail("Unrecognized MCP response shape")
	}, nil)
	swapKeyless(t, "parallel", func(context.Context, string, int) *WebSearchResponse { called = true; return searchOK(nil) }, nil)
	if out, _ := keylessSearchWithFailover(ctx, keylessPinnedEnv(t, "exa", nil), "exa", "q", 5); out.Success || called {
		t.Fatalf("non throttle error walked %+v", out)
	}
	for _, v := range keylessRing {
		swapKeyless(t, v, keylessThrottled(v), nil)
	}
	if out, _ := keylessSearchWithFailover(ctx, keylessPinnedEnv(t, "exa", nil), "exa", "q", 5); out.Success || !strings.Contains(out.Error, "all keyless vendors unavailable") {
		t.Fatalf("all throttled %+v", out)
	}
	swapKeyless(t, "firecrawl", keylessOK("firecrawl"), nil)
	if out, _ := keylessSearchWithFailover(ctx, keylessPinnedEnv(t, "exa", nil), "exa", "q", 5); !out.Success || out.data().ServedBy != "firecrawl" {
		t.Fatalf("multiple throttles %+v", out)
	}
	called = false
	swapKeyless(t, "exa", func(context.Context, string, int) *WebSearchResponse { called = true; return searchOK(nil) }, nil)
	swapKeyless(t, "firecrawl", keylessThrottled("firecrawl"), nil)
	if out, _ := keylessSearchWithFailover(ctx, keylessPinnedEnv(t, "parallel", map[string]any{"exa": "paid"}), "parallel", "q", 5); out.Success || called {
		t.Fatalf("paid peer walked %+v", out)
	}
	boom := errors.New("'list' object has no attribute 'get'")
	keylessSearchers["parallel"] = func(context.Context, string, int) (*WebSearchResponse, error) { return nil, boom }
	if _, err := keylessSearchWithFailover(ctx, keylessPinnedEnv(t, "parallel", nil), "parallel", "q", 5); !errors.Is(err, boom) {
		t.Fatalf("raised error not propagated %v", err)
	}
}

func TestKeylessExtractFailover(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	extractor := func(pages []*WebPage, called *bool) func(context.Context, []string) ([]*WebPage, error) {
		return func(context.Context, []string) ([]*WebPage, error) {
			if called != nil {
				*called = true
			}
			return pages, nil
		}
	}
	throttled := []*WebPage{{URL: "https://a", Error: "rate limit hit"}, {URL: "https://b", Error: "429 too many requests"}}
	good := []*WebPage{{URL: "https://a", Content: "A"}, {URL: "https://b", Content: "B"}}
	swapKeyless(t, "exa", nil, extractor(throttled, nil))
	swapKeyless(t, "parallel", nil, extractor(good, nil))
	if out, err := keylessExtractWithFailover(ctx, keylessPinnedEnv(t, "exa", nil), "exa", []string{"https://a", "https://b"}); err != nil || len(out) != 2 || out[0] != good[0] {
		t.Fatalf("throttled extract %+v %v", out, err)
	}
	forbidden := []*WebPage{{URL: "https://a", Error: "HTTP 403"}, {URL: "https://b", Error: "HTTP 403"}}
	called := false
	swapKeyless(t, "firecrawl", nil, extractor(forbidden, nil))
	swapKeyless(t, "keenable", nil, extractor(nil, &called))
	if out, _ := keylessExtractWithFailover(ctx, keylessPinnedEnv(t, "firecrawl", nil), "firecrawl", []string{"https://a", "https://b"}); out[0] != forbidden[0] || called {
		t.Fatalf("forbidden extract walked %+v", out)
	}
	partial := []*WebPage{{URL: "https://a", Content: "ok"}, {URL: "https://b", Error: "rate limit"}}
	called = false
	swapKeyless(t, "exa", nil, extractor(partial, nil))
	swapKeyless(t, "parallel", nil, extractor(nil, &called))
	if out, _ := keylessExtractWithFailover(ctx, keylessPinnedEnv(t, "exa", nil), "exa", []string{"https://a", "https://b"}); out[0] != partial[0] || called {
		t.Fatalf("partial extract walked %+v", out)
	}
}

func TestKeylessTierAndRingMembership(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	paid := keylessPinnedEnv(t, "", map[string]any{"exa": "paid", "parallel": "paid"})
	for _, name := range []string{"parallel", "exa"} {
		if p := webProvider(paid, name); p == nil || p.KeylessAvailable(paid) {
			t.Fatalf("%s keyless under a paid tier", name)
		}
	}
	tiers := keylessPinnedEnv(t, "", map[string]any{"exa": "FREE", "parallel": "bogus"})
	if providerTier(tiers, "exa") != "free" || providerTier(tiers, "parallel") != "auto" || providerTier(tiers, "keenable") != "auto" {
		t.Fatal("provider tier read")
	}
	order := keylessPreference()
	seen := map[string]bool{}
	for _, v := range order {
		seen[v] = true
	}
	if len(order) != len(keylessRing) || len(seen) != len(keylessRing) {
		t.Fatalf("preference %v", order)
	}
	unpinned := keylessPinnedEnv(t, "", nil)
	starts := map[string]bool{}
	for range keylessRing {
		starts[ringOrder(unpinned, "exa")[0]] = true
	}
	if len(starts) != len(keylessRing) {
		t.Fatalf("round robin starts %v", starts)
	}
	pinned := keylessPinnedEnv(t, "keenable", nil)
	if ringOrder(pinned, "keenable")[0] != "keenable" || ringOrder(pinned, "keenable")[0] != "keenable" {
		t.Fatal("pinned vendor does not lead")
	}
	_, inSearchers := keylessSearchers["tavily"]
	_, inExtractors := keylessExtractors["tavily"]
	if containsString(keylessRing, "tavily") || inSearchers || inExtractors || containsString(keylessPreference(), "tavily") {
		t.Fatal("tavily joined the keyless ring")
	}
}

func TestGetBackendAndAPIKeyGateWithTheKeylessTier(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, k := range []string{"FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "TAVILY_API_KEY", "KEENABLE_API_KEY", "BRAVE_SEARCH_API_KEY", "SEARXNG_URL", "TOOL_GATEWAY_USER_TOKEN"} {
		t.Setenv(k, "")
	}
	swapDDGS(t, &ddgsClientAvailable, func(context.Context) bool { return false })
	env := sdkProviderEnv(t, "", nil)
	want := keylessPreference()[0]
	if got := getWebBackend(env); got != want {
		t.Fatalf("keyless backend %q want %q", got, want)
	}
	if !CheckWebAPIKey(env) {
		t.Fatal("keyless install did not light the gate")
	}
	keyed := sdkProviderEnv(t, "EXA_API_KEY=sk-x\n", nil)
	if got := getWebBackend(keyed); got != "exa" {
		t.Fatalf("keyed backend %q", got)
	}
	disabled := sdkProviderEnv(t, "", map[string]any{"web.keyless_fallback": false})
	if got := getWebBackend(disabled); got != "firecrawl" {
		t.Fatalf("disabled backend %q", got)
	}
	if CheckWebAPIKey(disabled) {
		t.Fatal("disabled keyless tier lit the gate")
	}
}
