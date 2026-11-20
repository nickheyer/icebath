package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
)

const originStubHTML = "<!DOCTYPE html><html><body></body></html>"

// Resolves every host to a public address so the SSRF gate passes the test targets
func stubPublicHosts(t *testing.T) {
	t.Helper()
	prev := lookupHostIPs
	lookupHostIPs = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	t.Cleanup(func() { lookupHostIPs = prev })
}

// One keyed Firecrawl scrape of the test target answering data
func firecrawlScrapeEntry(t *testing.T, data string) *WebPage {
	t.Helper()
	stubPublicHosts(t)
	var body sharedText
	body.set(`{"success": true, "data": ` + data + `}`)
	srv := fakeFirecrawlScrape(t, &body)
	env := clearedWebEnv(t, nil)
	t.Setenv("FIRECRAWL_API_KEY", "test-key")
	t.Setenv("FIRECRAWL_API_URL", srv.URL)
	pages, err := webProvider(env, "firecrawl").Extract(context.Background(), env, []string{"https://t.test/x"}, "")
	if err != nil || len(pages) != 1 {
		t.Fatalf("%+v %v", pages, err)
	}
	return pages[0]
}

func TestFirecrawlScrapeFlagsOriginErrorStatus(t *testing.T) {
	for metadata, want := range map[string]string{
		`{"status_code": 403, "error": "Forbidden", "source_url": "https://t.test/x"}`: "HTTP 403 from origin: Forbidden",
		`{"statusCode": 404, "sourceURL": "https://t.test/x"}`:                         "HTTP 404 from origin",
	} {
		entry := firecrawlScrapeEntry(t, `{"markdown": "", "html": "`+originStubHTML+`", "metadata": `+metadata+`}`)
		if entry.Error != want || entry.Content != originStubHTML {
			t.Fatalf("%s: %+v", metadata, entry)
		}
	}
}

func TestFirecrawlScrapeSuccessHasNoError(t *testing.T) {
	entry := firecrawlScrapeEntry(t, `{"markdown": "# Hi", "metadata": {"status_code": 200, "title": "Hi"}}`)
	if entry.Content != "# Hi" || entry.Error != "" {
		t.Fatalf("%+v", entry)
	}
}

func TestKeylessFirecrawlFlagsOriginErrorStatus(t *testing.T) {
	var body sharedText
	body.set(`{"success": true, "data": {"markdown": "", "metadata": {"statusCode": 403, "error": "Forbidden"}}}`)
	srv := fakeFirecrawlScrape(t, &body)
	prev := firecrawlCloudAPIURL
	firecrawlCloudAPIURL = srv.URL
	t.Cleanup(func() { firecrawlCloudAPIURL = prev })
	out, err := firecrawlExtractKeyless(context.Background(), []string{"https://t.test/x"})
	if err != nil || len(out) != 1 || out[0].Error != "HTTP 403 from origin: Forbidden" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestKeylessParallelNamesOriginStatus(t *testing.T) {
	payload := `{"results": [], "errors": [{"url": "https://t.test/x", "error_type": "http_error", "http_status_code": 403, "content": null}]}`
	fakeMCPEndpoint(t, &parallelMCPURL, func() string { return mcpTextResult(t, payload) })
	out, err := parallelExtractKeyless(context.Background(), []string{"https://t.test/x"})
	if err != nil || len(out) != 1 || out[0].Error != "HTTP 403 from origin" || out[0].Content != "" {
		t.Fatalf("%+v %v", out, err)
	}
}

// Extract only provider outside the keyless ring answering one error per url
type erroringExtractProvider struct {
	baseWebProvider
	errors []string
}

func (p *erroringExtractProvider) Extract(_ context.Context, _ *Env, urls []string, _ string) ([]*WebPage, error) {
	out := make([]*WebPage, 0, len(urls))
	for i, u := range urls {
		out = append(out, pageError(u, p.errors[i]))
	}
	return out, nil
}

func TestKeylessRescueRunsForBackendFailuresNotOriginErrors(t *testing.T) {
	urls := []string{"https://t.test/a", "https://t.test/b"}
	for _, c := range []struct {
		errors  []string
		rescued bool
	}{
		{[]string{"HTTP 404 from origin", "HTTP 410 from origin: Gone"}, false},
		{[]string{"HTTP 500 upstream exploded", "HTTP 500 upstream exploded"}, true},
	} {
		env := sdkProviderEnv(t, "", nil)
		var ringCalls atomic.Int32
		for _, v := range keylessRing {
			swapKeyless(t, v, nil, func(context.Context, []string) ([]*WebPage, error) { ringCalls.Add(1); return []*WebPage{}, nil })
		}
		provider := &erroringExtractProvider{baseWebProvider{name: "origin-test", display: "Origin test", extract: true}, c.errors}
		if !rescueEligible(env, provider) {
			t.Fatal("a provider outside the ring is rescue eligible")
		}
		out, _, err := fetchWithRescue(webCallContext{ctx: context.Background(), env: env}, provider, urls, "")
		if err != nil || (ringCalls.Load() > 0) != c.rescued {
			t.Fatalf("%v: ring calls %d %v", c.errors, ringCalls.Load(), err)
		}
		if !c.rescued && (len(out) != 2 || out[0].Error != c.errors[0] || out[1].Error != c.errors[1]) {
			t.Fatalf("origin errors are returned as is: %+v", out)
		}
	}
}

func TestOrigin429DoesNotWalkTheKeylessExtractRing(t *testing.T) {
	env := sdkProviderEnv(t, "", map[string]any{"web.provider_tier.parallel": "free", "web.provider_tier.exa": "paid", "web.provider_tier.keenable": "paid"})
	if got := ringOrder(env, "parallel"); !reflect.DeepEqual(got, []string{"parallel", "firecrawl"}) {
		t.Fatalf("%v", got)
	}
	var calls []string
	extractor := func(vendor, errText string) func(context.Context, []string) ([]*WebPage, error) {
		return func(_ context.Context, urls []string) ([]*WebPage, error) {
			calls = append(calls, vendor)
			return extractFail(urls, errText), nil
		}
	}
	swapKeyless(t, "parallel", nil, extractor("parallel", "HTTP 429 from origin"))
	swapKeyless(t, "firecrawl", nil, extractor("firecrawl", "Firecrawl: other error"))
	out, err := keylessExtractWithFailover(context.Background(), env, "parallel", []string{"https://t.test/x"})
	if err != nil || !reflect.DeepEqual(calls, []string{"parallel"}) || out[0].Error != "HTTP 429 from origin" {
		t.Fatalf("%v %+v %v", calls, out, err)
	}
	calls = nil
	keylessExtractors["parallel"] = extractor("parallel", "HTTP 429: Too Many Requests")
	out, _ = keylessExtractWithFailover(context.Background(), env, "parallel", []string{"https://t.test/x"})
	if !reflect.DeepEqual(calls, []string{"parallel", "firecrawl"}) || out[0].Error != "Firecrawl: other error" {
		t.Fatalf("%v %+v", calls, out)
	}
}

func TestVendorReportedOriginStatusDoesNotWalkTheKeylessExtractRing(t *testing.T) {
	stubPublicHosts(t)
	env := sdkProviderEnv(t, "", map[string]any{"web.provider_tier.keenable": "free", "web.provider_tier.exa": "free", "web.provider_tier.parallel": "paid", "web.provider_tier.firecrawl": "paid"})
	if !reflect.DeepEqual(ringOrder(env, "keenable"), []string{"keenable", "exa"}) || !reflect.DeepEqual(ringOrder(env, "exa"), []string{"exa", "keenable"}) {
		t.Fatal("ring order")
	}
	target429 := `{"error": "Rate limit exceeded", "message": "The target server rate-limited this fetch, please retry shortly."}`
	own429 := `{"error": "Rate limit exceeded", "message": "Too many requests, slow down."}`
	var keenableBody sharedText
	keenableBody.set(target429)
	var keenableGets, exaPosts atomic.Int32
	keenable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keenableGets.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, keenableBody.get())
	}))
	t.Cleanup(keenable.Close)
	prevK := keenableAPIURL
	keenableAPIURL = keenable.URL
	t.Cleanup(func() { keenableAPIURL = prevK })
	exaCrawl429 := `data: {"jsonrpc": "2.0", "id": 1, "result": {"isError": true, "content": [{"type": "text", "text": "Error fetching URL(s): https://t.test/x: CRAWL_HTTP_429"}]}}`
	exa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exaPosts.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, exaCrawl429)
	}))
	t.Cleanup(exa.Close)
	prevE := exaMCPURL
	exaMCPURL = exa.URL
	t.Cleanup(func() { exaMCPURL = prevE })
	url := []string{"https://t.test/x"}
	out, err := keylessExtractWithFailover(context.Background(), env, "keenable", url)
	if err != nil || out[0].Error != "HTTP 429 from origin: The target server rate-limited this fetch, please retry shortly." || keenableGets.Load() != 1 || exaPosts.Load() != 0 {
		t.Fatalf("%+v %v %d %d", out, err, keenableGets.Load(), exaPosts.Load())
	}
	out, err = keylessExtractWithFailover(context.Background(), env, "exa", url)
	if err != nil || out[0].Error != "HTTP 429 from origin" || exaPosts.Load() != 1 || keenableGets.Load() != 1 {
		t.Fatalf("%+v %v %d %d", out, err, exaPosts.Load(), keenableGets.Load())
	}
	keenableBody.set(own429)
	keenableGets.Store(0)
	exaPosts.Store(0)
	keylessExtractWithFailover(context.Background(), env, "keenable", url)
	if keenableGets.Load() != 1 || exaPosts.Load() != 1 {
		t.Fatalf("keenable throttling this client still fails over: %d %d", keenableGets.Load(), exaPosts.Load())
	}
}
