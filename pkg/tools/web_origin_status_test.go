package tools

import (
	"context"
	"errors"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestOriginHTTPErrorIntSemantics(t *testing.T) {
	huge, _ := new(big.Int).SetString("1000000000000000000000000000000", 10)
	cases := []struct {
		status, reason any
		want           string
	}{
		{true, nil, ""},
		{false, "x", ""},
		{"4_04", nil, "HTTP 404 from origin"},
		{"٤٠٤", nil, "HTTP 404 from origin"},
		{"4__04", nil, ""},
		{"_404", nil, ""},
		{" 503\n", "  busy ", "HTTP 503 from origin: busy"},
		{huge, nil, "HTTP 1000000000000000000000000000000 from origin"},
		{"1" + strings.Repeat("0", 30), nil, "HTTP 1000000000000000000000000000000 from origin"},
		{math.NaN(), nil, ""},
		{nil, "x", ""},
		{"abc", nil, ""},
		{[]any{404}, nil, ""},
		{399, nil, ""},
		{int64(400), 0, "HTTP 400 from origin"},
		{404.9, []any{"a"}, "HTTP 404 from origin: ['a']"},
		{-math.MaxFloat64, nil, ""},
	}
	for _, c := range cases {
		got, err := originHTTPError(c.status, c.reason)
		if err != nil || got != c.want {
			t.Fatalf("%#v %#v: %q %v", c.status, c.reason, got, err)
		}
	}
	for _, inf := range []float64{math.Inf(1), math.Inf(-1)} {
		_, err := originHTTPError(inf, nil)
		var pe *pyint.Error
		if !errors.As(err, &pe) || pe.Kind != "OverflowError" || err.Error() != "cannot convert float infinity to integer" {
			t.Fatalf("infinity must raise the OverflowError: %v", err)
		}
	}
	if _, err := withOriginStatus(&WebPage{}, math.Inf(1), nil); err == nil {
		t.Fatal("with_origin_status must raise what origin_http_error raises")
	}
	if _, err := vendorErrorEntry("u", math.Inf(1), nil, nil); err == nil {
		t.Fatal("vendor_error_entry must raise what origin_http_error raises")
	}
}

func TestParallelSDKExtractRaisesAnInfiniteOriginStatus(t *testing.T) {
	env := sdkProviderEnv(t, "PARALLEL_API_KEY=par-key\n", map[string]any{"web.keyless_fallback": false})
	var body sharedText
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) { writeVendorJSON(w, 200, body.get()) })
	t.Setenv("PARALLEL_BASE_URL", srv.URL)
	urls := []string{"https://t.test/x", "https://t.test/y"}
	body.set(`{"extract_id":"e","results":[],"errors":[{"url":"https://t.test/x","error_type":"http_error","http_status_code":Infinity,"content":"nope"}]}`)
	out, _ := parallelP().Extract(context.Background(), env, urls, "")
	if len(out) != 2 || out[1].URL != "https://t.test/y" || out[0].Error != "Parallel extract failed: cannot convert float infinity to integer" || out[1].Error != out[0].Error {
		t.Fatalf("the guard turns the OverflowError into an extract failure for every url: %+v", out)
	}
	body.set(`{"extract_id":"e","results":[],"errors":[{"url":"https://t.test/x","error_type":"http_error","http_status_code":NaN,"content":"nope"},{"url":"https://t.test/y","error_type":"http_error","http_status_code":true}]}`)
	out, _ = parallelP().Extract(context.Background(), env, urls, "")
	if len(out) != 2 || out[0].Error != "nope" || out[1].Error != "http_error" {
		t.Fatalf("NaN and bool statuses fall back to the vendor body or reason: %+v", out)
	}
}

func TestExaSDKExtractNamesTheCrawlStatus(t *testing.T) {
	env := sdkProviderEnv(t, "EXA_API_KEY=exa-key\n", map[string]any{"web.keyless_fallback": false})
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[],"statuses":[{"id":"https://t.test/a","status":"error","error":{"tag":"CRAWL_HTTP_403","httpStatusCode":403}},{"id":"https://t.test/b","status":"error","error":{"tag":"CRAWL_LIVECRAWL_TIMEOUT"}}]}`)
	})
	prev := exaBaseURL
	exaBaseURL = srv.URL
	t.Cleanup(func() { exaBaseURL = prev })
	out, _ := exaP().Extract(context.Background(), env, []string{"https://t.test/a", "https://t.test/b"}, "")
	if len(out) != 2 || out[0].Error != "HTTP 403 from origin" || out[1].Error != "CRAWL_LIVECRAWL_TIMEOUT" {
		t.Fatalf("%+v", out)
	}
}

func TestExaSDKExtractRaisesAnInfiniteHTTPStatusCode(t *testing.T) {
	env := sdkProviderEnv(t, "EXA_API_KEY=exa-key\n", map[string]any{"web.keyless_fallback": false})
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[],"statuses":[{"id":"https://t.test/a","status":"error","error":{"tag":"CRAWL_HTTP_403","httpStatusCode":true}},{"id":"https://t.test/b","status":"error","error":{"tag":"X","httpStatusCode":Infinity}}]}`)
	})
	prev := exaBaseURL
	exaBaseURL = srv.URL
	t.Cleanup(func() { exaBaseURL = prev })
	out, _ := exaP().Extract(context.Background(), env, []string{"https://t.test/a", "https://t.test/b"}, "")
	if len(out) != 2 || out[0].Error != "Exa extract failed: cannot convert float infinity to integer" || out[1].Error != out[0].Error {
		t.Fatalf("%+v", out)
	}
}

// Local MCP endpoint answering every tools/call with reply
func fakeMCPEndpoint(t *testing.T, target *string, reply func() string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply())
	}))
	t.Cleanup(srv.Close)
	prev := *target
	*target = srv.URL
	t.Cleanup(func() { *target = prev })
}

// JSON-RPC result whose single text content is text
func mcpTextResult(t *testing.T, text string) string {
	t.Helper()
	encoded, err := pyjson.Dumps(text)
	if err != nil {
		t.Fatal(err)
	}
	return `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":` + encoded + `}]}}`
}

func TestParallelKeylessExtractPythonPayloadSemantics(t *testing.T) {
	var payload sharedText
	fakeMCPEndpoint(t, &parallelMCPURL, func() string { return mcpTextResult(t, payload.get()) })
	urls := []string{"https://t.test/a", "https://t.test/b"}
	payload.set(`{"results":[{"url":"https://t.test/a","title":"A","excerpts":["x","y"]}],"errors":[{"url":"https://t.test/b","error_type":"http_error","http_status_code":"4_04","content":"gone"}]}`)
	out, err := parallelExtractKeyless(context.Background(), urls)
	if err != nil || len(out) != 2 || out[0].Content != "x\n\ny" || out[1].Error != "HTTP 404 from origin" || out[1].Content != "gone" {
		t.Fatalf("%+v %v", out, err)
	}
	for text, want := range map[string]string{
		`{"results":[],"errors":[{"url":"https://t.test/a","http_status_code":Infinity}]}`: "cannot convert float infinity to integer",
		`[1, 2]`:                           "'list' object has no attribute 'get'",
		`{"results":["https://t.test/a"]}`: "'str' object has no attribute 'get'",
		`{"results":[{"excerpts":[1]}]}`:   "sequence item 0: expected str instance, int found",
	} {
		payload.set(text)
		if _, err := parallelExtractKeyless(context.Background(), urls); err == nil || err.Error() != want {
			t.Fatalf("%s: %v", text, err)
		}
	}
	payload.set("not json")
	out, err = parallelExtractKeyless(context.Background(), urls)
	if err != nil || len(out) != 2 || !strings.HasPrefix(out[1].Error, "Keyless Parallel extract failed: Expecting value: line 1 column 1 (char 0). Set PARALLEL_API_KEY") {
		t.Fatalf("%+v %v", out, err)
	}
	env := sdkProviderEnv(t, "", map[string]any{"web.provider_tier.parallel": "free"})
	payload.set(`{"errors":[{"url":"https://t.test/a","http_status_code":-Infinity}]}`)
	out, _ = parallelP().Extract(context.Background(), env, urls, "")
	if len(out) != 2 || out[0].Error != "Parallel extract failed: cannot convert float infinity to integer" {
		t.Fatalf("the raised OverflowError reaches the provider guard: %+v", out)
	}
}

func TestExaKeylessExtractCrawlStatusBranch(t *testing.T) {
	var reply sharedText
	fakeMCPEndpoint(t, &exaMCPURL, reply.get)
	reply.set(`{"jsonrpc":"2.0","id":1,"error":{"message":"fetch failed: CRAWL_HTTP_429"}}`)
	out, err := exaExtractKeyless(context.Background(), []string{"https://t.test/a"})
	if err != nil || len(out) != 1 || out[0].Error != "HTTP 429 from origin" {
		t.Fatalf("%+v %v", out, err)
	}
	reply.set(`{"jsonrpc":"2.0","id":1,"error":{"message":"fetch failed: CRAWL_NOT_FOUND"}}`)
	if out, _ = exaExtractKeyless(context.Background(), []string{"https://t.test/a"}); out[0].Error != "HTTP 404 from origin" {
		t.Fatalf("%+v", out)
	}
	reply.set(`{"jsonrpc":"2.0","id":1,"error":{"message":"rate limited"}}`)
	out, err = exaExtractKeyless(context.Background(), []string{"https://t.test/a"})
	if err != nil || !strings.HasPrefix(out[0].Error, "Keyless Exa extract failed: rate limited. Set EXA_API_KEY (https://exa.ai) or another web backend") {
		t.Fatalf("%+v %v", out, err)
	}
	reply.set(`{"jsonrpc": broken`)
	if out, err = exaExtractKeyless(context.Background(), []string{"https://t.test/a"}); err != nil || !strings.HasPrefix(out[0].Error, "Keyless Exa extract failed: Unrecognized MCP response shape.") {
		t.Fatalf("an undecodable envelope is skipped into the unrecognized shape KeylessMCPError: %+v %v", out, err)
	}
	reply.set(`{"jsonrpc":"2.0","id":1,"error":"denied"}`)
	if _, err = exaExtractKeyless(context.Background(), []string{"https://t.test/a"}); err == nil || isKeylessMCPError(err) || err.Error() != "'str' object has no attribute 'get'" {
		t.Fatalf("an AttributeError is not a KeylessMCPError and escapes the per url catch: %v", err)
	}
	reply.set(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"text":7}]}}`)
	if _, err = exaExtractKeyless(context.Background(), []string{"https://t.test/a"}); err == nil || err.Error() != "sequence item 0: expected str instance, int found" {
		t.Fatalf("%v", err)
	}
	reply.set(`{"jsonrpc":"2.0","id":1,"error":{"code":-1}}`)
	if out, _ = exaExtractKeyless(context.Background(), []string{"https://t.test/a"}); !strings.HasPrefix(out[0].Error, "Keyless Exa extract failed: {'code': -1}.") {
		t.Fatalf("%+v", out)
	}
}

// String shared between a test and its fake server handlers
type sharedText struct{ v atomic.Value }

func (s *sharedText) set(x string) { s.v.Store(x) }

func (s *sharedText) get() string {
	x, _ := s.v.Load().(string)
	return x
}

// Firecrawl endpoint answering every scrape with the current body
func fakeFirecrawlScrape(t *testing.T, body *sharedText) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body.get())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFirecrawlKeylessExtractCatchesAnInfiniteStatus(t *testing.T) {
	var body sharedText
	body.set(`{"success": true, "data": {"metadata": {"statusCode": Infinity}, "markdown": "x"}}`)
	srv := fakeFirecrawlScrape(t, &body)
	prev := firecrawlCloudAPIURL
	firecrawlCloudAPIURL = srv.URL
	t.Cleanup(func() { firecrawlCloudAPIURL = prev })
	out, err := firecrawlExtractKeyless(context.Background(), []string{"https://t.test/a"})
	want := "Keyless Firecrawl extract failed: cannot convert float infinity to integer. Set FIRECRAWL_API_KEY (https://firecrawl.dev) for reliable service."
	if err != nil || len(out) != 1 || out[0].URL != "https://t.test/a" || out[0].Error != want {
		t.Fatalf("%+v %v", out, err)
	}
	body.set(`{"success": true, "data": {"metadata": {"status_code": "4_04", "error": " Not Found "}, "markdown": "x"}}`)
	if out, _ = firecrawlExtractKeyless(context.Background(), []string{"https://t.test/a"}); out[0].Error != "HTTP 404 from origin: Not Found" || out[0].Content != "x" {
		t.Fatalf("%+v", out[0])
	}
}

func TestFirecrawlScrapeOneReportsAnInfiniteStatus(t *testing.T) {
	stubPublicHosts(t)
	var body sharedText
	body.set(`{"success": true, "data": {"metadata": {"title": "T", "statusCode": Infinity}, "markdown": "content"}}`)
	srv := fakeFirecrawlScrape(t, &body)
	stubPublicHosts(t)
	env := clearedWebEnv(t, nil)
	t.Setenv("FIRECRAWL_API_KEY", "test-key")
	t.Setenv("FIRECRAWL_API_URL", srv.URL)
	pages, err := webProvider(env, "firecrawl").Extract(context.Background(), env, []string{"https://example.com"}, "")
	if err != nil || len(pages) != 1 {
		t.Fatalf("%+v %v", pages, err)
	}
	p := pages[0]
	if p.URL != "https://example.com" || p.Error != "cannot convert float infinity to integer" || p.Title != "" || p.Content != "" || !p.HasRaw {
		t.Fatalf("the scrape except clause returns the bare error entry with raw set: %+v", p)
	}
}

func TestKeylessRingErrorReachesTheRescueCaller(t *testing.T) {
	env := sdkProviderEnv(t, "", map[string]any{"web.provider_tier.parallel": "free"})
	raised := errors.New("cannot convert float infinity to integer")
	swapKeyless(t, "parallel", nil, func(context.Context, []string) ([]*WebPage, error) { return nil, raised })
	if _, err := keylessExtractWithFailover(context.Background(), env, "parallel", []string{"https://t.test/a"}); err != raised {
		t.Fatalf("%v", err)
	}
	failed := []*WebPage{pageError("https://t.test/a", "HTTP 500: down")}
	if _, err := rescueExtract(webCallContext{ctx: context.Background(), env: env}, "parallel", []string{"https://t.test/a"}, failed); err != raised {
		t.Fatalf("%v", err)
	}
	if _, rescued, err := rescuedExtract(nil, raised); err != raised || rescued {
		t.Fatalf("%v %v", rescued, err)
	}
}
