package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Request one fake vendor server saw
type vendorCall struct {
	path   string
	header http.Header
	body   map[string]any
	raw    string
}

// Fake vendor server answering each request through the handler and recording it
func fakeVendor(t *testing.T, handler func(call vendorCall, w http.ResponseWriter)) (*httptest.Server, *[]vendorCall) {
	t.Helper()
	var mu sync.Mutex
	calls := &[]vendorCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := vendorCall{path: r.URL.Path, header: r.Header.Clone(), raw: string(raw)}
		json.Unmarshal(raw, &call.body)
		mu.Lock()
		*calls = append(*calls, call)
		mu.Unlock()
		handler(call, w)
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

// Writes a JSON answer with the given status
func writeVendorJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// Web provider env with its own home, no vendor keys in the process and empty client slots
func sdkProviderEnv(t *testing.T, dotenv string, settings map[string]any) *Env {
	t.Helper()
	for _, k := range []string{"EXA_API_KEY", "PARALLEL_API_KEY", "PARALLEL_BASE_URL", "PARALLEL_CUSTOM_HEADERS", "PARALLEL_SEARCH_MODE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	h := t.TempDir()
	if dotenv != "" {
		if err := os.WriteFile(filepath.Join(h, ".env"), []byte(dotenv), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	for k, v := range settings {
		cfg.Set(k, v)
	}
	resetSDKClientSlots(t)
	return &Env{Home: h, Config: cfg, Secrets: config.NewSecrets(h), Values: &sync.Map{}}
}

// Empties every SDK client slot before and after the test
func resetSDKClientSlots(t *testing.T) {
	clear := func() {
		sdkClientSlots.Lock()
		sdkClientSlots.bySlot = map[string]sdkClientSlot{}
		sdkClientSlots.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// Points the Exa client at a fake server
func useExaServer(t *testing.T, srv *httptest.Server) {
	prev := exaBaseURL
	exaBaseURL = srv.URL
	t.Cleanup(func() { exaBaseURL = prev })
}

// Records Parallel backoff sleeps instead of sleeping
func recordParallelSleeps(t *testing.T) *[]time.Duration {
	prev := parallelSleep
	sleeps := &[]time.Duration{}
	parallelSleep = func(ctx context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
	t.Cleanup(func() { parallelSleep = prev })
	return sleeps
}

// Swaps one keyless searcher and extractor for the test
func swapKeyless(t *testing.T, name string, search func(context.Context, string, int) *WebSearchResponse, extract func(context.Context, []string) ([]*WebPage, error)) {
	prevS, prevE := keylessSearchers[name], keylessExtractors[name]
	if search != nil {
		keylessSearchers[name] = func(ctx context.Context, query string, limit int) (*WebSearchResponse, error) {
			return search(ctx, query, limit), nil
		}
	}
	if extract != nil {
		keylessExtractors[name] = extract
	}
	t.Cleanup(func() { keylessSearchers[name], keylessExtractors[name] = prevS, prevE })
}

func exaP() *exaProvider           { return webProvider(nil, "exa").(*exaProvider) }
func parallelP() *parallelProvider { return webProvider(nil, "parallel").(*parallelProvider) }

func TestExaSDKReportsFailedURLsFromStatuses(t *testing.T) {
	env := sdkProviderEnv(t, "EXA_API_KEY=exa-key\n", map[string]any{"web.keyless_fallback": false})
	srv, calls := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[{"url":"https://t.test/ok","title":"OK","text":"body"}],"statuses":[`+
			`{"id":"https://t.test/ok","status":"success"},{"id":"https://t.test/x","status":"error","error":{"tag":"CRAWL_HTTP_403","httpStatusCode":403}}]}`)
	})
	useExaServer(t, srv)
	out, err := exaP().Extract(context.Background(), env, []string{"https://t.test/ok", "https://t.test/x"}, "")
	if err != nil || len(out) != 2 {
		t.Fatalf("%v %v", out, err)
	}
	if out[0].Error != "" || out[0].Content != "body" || out[1].Error != "HTTP 403 from origin" || out[1].URL != "https://t.test/x" {
		t.Fatalf("%+v %+v", out[0], out[1])
	}
	c := (*calls)[0]
	if c.path != "/contents" || c.header.Get("x-api-key") != "exa-key" || c.header.Get("x-exa-integration") != "icebath-agent" {
		t.Fatalf("%+v", c)
	}
	if c.raw != `{"urls": ["https://t.test/ok", "https://t.test/x"], "text": true}` {
		t.Fatalf("body %s", c.raw)
	}
}

func TestExaCrawlTagNamesTheOriginStatus(t *testing.T) {
	env := sdkProviderEnv(t, "EXA_API_KEY=exa-key\n", map[string]any{"web.keyless_fallback": false})
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[],"statuses":[{"id":"https://t.test/a","status":"error","error":{"tag":"CRAWL_NOT_FOUND"}},`+
			`{"id":"https://t.test/b","status":"error","error":{"tag":"SOURCE_NOT_AVAILABLE"}},{"id":"https://t.test/c","status":"error"}]}`)
	})
	useExaServer(t, srv)
	out, _ := exaP().Extract(context.Background(), env, []string{"https://t.test/a", "https://t.test/b", "https://t.test/c"}, "")
	got := []string{out[0].Error, out[1].Error, out[2].Error}
	if want := []string{"HTTP 404 from origin", "SOURCE_NOT_AVAILABLE", "extraction failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestExaSearchSendsHighlightsAndJoinsThem(t *testing.T) {
	env := sdkProviderEnv(t, "EXA_API_KEY=exa-key\n", map[string]any{"web.keyless_fallback": false})
	srv, calls := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"results":[{"url":"https://a.test","title":"A","highlights":["one","two"]},{"url":null,"title":null}],`+
			`"warnings":[{"code":"ignored_field","path":"/x","message":"unused"}]}`)
	})
	useExaServer(t, srv)
	resp, err := exaP().Search(context.Background(), env, "hot ai", 7)
	if err != nil || !resp.Success {
		t.Fatalf("%+v %v", resp, err)
	}
	if want := []WebSearchHit{webHit("https://a.test", "A", "one two", 1), webHit("", "", "", 2)}; !reflect.DeepEqual(resp.Data.Web, want) {
		t.Fatalf("%+v", resp.Data.Web)
	}
	if raw := (*calls)[0].raw; raw != `{"query": "hot ai", "contents": {"highlights": true}, "numResults": 7}` {
		t.Fatalf("body %s", raw)
	}
}

func TestExaFailuresKeepTheSDKMessages(t *testing.T) {
	env := sdkProviderEnv(t, "EXA_API_KEY=exa-key\n", map[string]any{"web.keyless_fallback": false})
	answer := ""
	status := 200
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		io.WriteString(w, answer)
	})
	useExaServer(t, srv)
	cases := []struct {
		status int
		answer string
		want   string
	}{
		{401, `{"error":"bad key"}`, `Request failed with status code 401: {"error":"bad key"}`},
		{200, "<html>\n  oops </html>", "Expected a JSON response but received a non-JSON body (status 200): <html> oops </html>"},
		{200, "", "Expected a JSON response but received an empty body (status 200)."},
		{200, `{"statuses":[]}`, "Exa search failed: 'results'"},
		{200, `[1]`, "Exa search failed: 'list' object has no attribute 'get'"},
		{200, `{"results":["x"]}`, "Exa search failed: 'str' object has no attribute 'get'"},
		{200, `{"results":[{"highlights":[1]}]}`, "Exa search failed: sequence item 0: expected str instance, int found"},
	}
	for _, c := range cases {
		status, answer = c.status, c.answer
		resp, _ := exaP().Search(context.Background(), env, "q", 5)
		if resp.Success || resp.Error != c.want {
			t.Fatalf("%q -> %q want %q", c.answer, resp.Error, c.want)
		}
	}
}

func TestExaMissingKeyIsReportedVerbatim(t *testing.T) {
	env := sdkProviderEnv(t, "", map[string]any{"web.keyless_fallback": false})
	resp, _ := exaP().Search(context.Background(), env, "q", 5)
	if resp.Success || resp.Error != exaMissingKey {
		t.Fatalf("%+v", resp)
	}
	pages, _ := exaP().Extract(context.Background(), env, []string{"https://a"}, "")
	if pages[0].Error != exaMissingKey {
		t.Fatalf("%+v", pages[0])
	}
}

// Profile secret source whose bound scope rides on the context
type ctxProfileSecrets struct{ testProfileSecrets }

type scopeKey struct{}

func (ctxProfileSecrets) CurrentSecretScope(ctx context.Context) map[string]string {
	m, _ := ctx.Value(scopeKey{}).(map[string]string)
	return m
}

func TestMultiplexedProfilesSearchWithTheirOwnKey(t *testing.T) {
	launch := sdkProviderEnv(t, "", map[string]any{"web.keyless_fallback": false})
	t.Setenv("EXA_API_KEY", "key-launch")
	prevP := ProfileSecrets
	ProfileSecrets = ctxProfileSecrets{}
	SetMultiplexActive(true)
	t.Cleanup(func() { ProfileSecrets = prevP; SetMultiplexActive(false) })
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, fmt.Sprintf(`{"results":[{"url":"https://example.com","title":"t","highlights":[%q]}]}`, call.header.Get("x-api-key")))
	})
	useExaServer(t, srv)
	homes := map[string]string{}
	for name, line := range map[string]string{"a": "EXA_API_KEY=key-a\n", "b": "EXA_API_KEY=key-b\n", "nokey": ""} {
		homes[name] = filepath.Join(t.TempDir(), name)
		os.MkdirAll(homes[name], 0o755)
		os.WriteFile(filepath.Join(homes[name], ".env"), []byte(line), 0o600)
	}
	sentWith := func(name string) string {
		ctx := context.WithValue(context.Background(), scopeKey{}, ProfileSecrets.BuildProfileSecretScope(homes[name]))
		resp, _ := exaP().Search(ctx, launch, "q", 5)
		if !resp.Success {
			return resp.Error
		}
		return resp.Data.Web[0].Description
	}
	if got := []string{sentWith("a"), sentWith("b"), sentWith("a")}; !reflect.DeepEqual(got, []string{"key-a", "key-b", "key-a"}) {
		t.Fatalf("%v", got)
	}
	if got := sentWith("nokey"); !strings.Contains(got, "EXA_API_KEY") || strings.Contains(got, "key-launch") {
		t.Fatalf("nokey %q", got)
	}
	if got := sentWith("a"); got != "key-a" {
		t.Fatalf("%q", got)
	}
	if resp, _ := exaP().Search(context.Background(), &Env{Home: launch.Home, Config: launch.Config}, "q", 5); resp.Success || !strings.Contains(resp.Error, ErrUnscopedSecret.Error()) {
		t.Fatalf("unscoped %+v", resp)
	}
}

func TestSingleProfileProcessEnvStillServesTheKey(t *testing.T) {
	env := sdkProviderEnv(t, "", map[string]any{"web.keyless_fallback": false})
	t.Setenv("EXA_API_KEY", "key-env")
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, fmt.Sprintf(`{"results":[{"url":"https://example.com","title":"t","highlights":[%q]}]}`, call.header.Get("x-api-key")))
	})
	useExaServer(t, srv)
	resp, _ := exaP().Search(context.Background(), env, "q", 5)
	if !resp.Success || resp.Data.Web[0].Description != "key-env" {
		t.Fatalf("%+v", resp)
	}
}

func TestParallelSDKNamesOriginStatus(t *testing.T) {
	env := sdkProviderEnv(t, "PARALLEL_API_KEY=par-key\n", map[string]any{"web.keyless_fallback": false})
	srv, calls := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"extract_id":"e1","results":[],"errors":[{"url":"https://t.test/x","error_type":"http_error","http_status_code":404,"content":"nope"}]}`)
	})
	t.Setenv("PARALLEL_BASE_URL", srv.URL)
	out, _ := parallelP().Extract(context.Background(), env, []string{"https://t.test/x"}, "")
	if len(out) != 1 || out[0].Error != "HTTP 404 from origin" || out[0].Content != "nope" {
		t.Fatalf("%+v", out)
	}
	c := (*calls)[0]
	if c.path != "/v1beta/extract" || c.raw != `{"urls":["https://t.test/x"],"full_content":true}` {
		t.Fatalf("%+v", c)
	}
	for k, v := range map[string]string{"x-api-key": "par-key", "parallel-beta": "search-extract-2025-10-10", "X-Stainless-Async": "async:goroutine",
		"x-stainless-retry-count": "0", "x-stainless-read-timeout": "600", "Accept": "application/json"} {
		if c.header.Get(k) != v {
			t.Fatalf("%s=%q", k, c.header.Get(k))
		}
	}
}

func TestParallelExtractPrefersFullContentThenExcerpts(t *testing.T) {
	env := sdkProviderEnv(t, "PARALLEL_API_KEY=par-key\n", map[string]any{"web.keyless_fallback": false})
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"extract_id":"e","results":[{"url":"https://a","title":"A","full_content":"full"},{"url":"https://b","excerpts":["x","y"]}],"errors":[]}`)
	})
	t.Setenv("PARALLEL_BASE_URL", srv.URL+"/")
	out, _ := parallelP().Extract(context.Background(), env, []string{"https://a", "https://b"}, "")
	if len(out) != 2 || out[0].Content != "full" || out[0].Title != "A" || out[1].Content != "x\n\ny" || out[1].Metadata["sourceURL"] != "https://b" {
		t.Fatalf("%+v %+v", out[0], out[1])
	}
}

func TestParallelSearchCapsResultsAndReadsTheMode(t *testing.T) {
	env := sdkProviderEnv(t, "PARALLEL_API_KEY=par-key\n", map[string]any{"web.keyless_fallback": false})
	srv, calls := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"search_id":"s","results":[{"url":"https://a","title":"A","excerpts":["e1","e2"]},{"url":"https://b"}]}`)
	})
	t.Setenv("PARALLEL_BASE_URL", srv.URL)
	for mode, want := range map[string]string{"": "agentic", " FAST ": "fast", "one-shot": "one-shot", "bogus": "agentic"} {
		os.Unsetenv("PARALLEL_SEARCH_MODE")
		if mode != "" {
			t.Setenv("PARALLEL_SEARCH_MODE", mode)
		}
		resp, _ := parallelP().Search(context.Background(), env, "q", 50)
		if !resp.Success || !reflect.DeepEqual(resp.Data.Web, []WebSearchHit{webHit("https://a", "A", "e1 e2", 1), webHit("https://b", "", "", 2)}) {
			t.Fatalf("%+v", resp)
		}
		last := (*calls)[len(*calls)-1]
		if last.raw != fmt.Sprintf(`{"max_results":20,"mode":%q,"objective":"q","search_queries":["q"]}`, want) || last.header.Get("X-Stainless-Async") != "false" {
			t.Fatalf("%q -> %s", mode, last.raw)
		}
	}
}

func TestParallelRetriesLikeTheSDK(t *testing.T) {
	env := sdkProviderEnv(t, "PARALLEL_API_KEY=par-key\n", map[string]any{"web.keyless_fallback": false})
	sleeps := recordParallelSleeps(t)
	var n atomic.Int32
	srv, calls := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		switch n.Add(1) {
		case 1:
			w.Header().Set("retry-after-ms", "1500")
			writeVendorJSON(w, 429, `{"error":{"message":"slow down"}}`)
		case 2:
			writeVendorJSON(w, 503, `oops`)
		default:
			writeVendorJSON(w, 200, `{"search_id":"s","results":[]}`)
		}
	})
	t.Setenv("PARALLEL_BASE_URL", srv.URL)
	resp, _ := parallelP().Search(context.Background(), env, "q", 5)
	if !resp.Success || len(*calls) != 3 || (*sleeps)[0] != 1500*time.Millisecond || (*sleeps)[1] < 750*time.Millisecond || (*sleeps)[1] > time.Second {
		t.Fatalf("%+v calls %d sleeps %v", resp, len(*calls), *sleeps)
	}
	for i, c := range *calls {
		if c.header.Get("x-stainless-retry-count") != fmt.Sprint(i) {
			t.Fatalf("retry count %d %q", i, c.header.Get("x-stainless-retry-count"))
		}
	}
	srv2, calls2 := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		w.Header().Set("x-should-retry", "false")
		writeVendorJSON(w, 500, `{"error":{"message":"boom","type":"server"}}`)
	})
	t.Setenv("PARALLEL_BASE_URL", srv2.URL)
	resetSDKClientSlots(t)
	resp, _ = parallelP().Search(context.Background(), env, "q", 5)
	if want := "Parallel search failed: Error code: 500 - {'error': {'message': 'boom', 'type': 'server'}}"; resp.Error != want || len(*calls2) != 1 {
		t.Fatalf("%q calls %d", resp.Error, len(*calls2))
	}
}

func TestParallelStatusAndPayloadErrors(t *testing.T) {
	env := sdkProviderEnv(t, "PARALLEL_API_KEY=par-key\n", map[string]any{"web.keyless_fallback": false})
	recordParallelSleeps(t)
	status, ctype, answer := 0, "", ""
	srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		io.WriteString(w, answer)
	})
	t.Setenv("PARALLEL_BASE_URL", srv.URL)
	cases := []struct {
		status        int
		ctype, answer string
		want          string
	}{
		{401, "application/json", `{"error":"bad key"}`, "Parallel search failed: Error code: 401 - {'error': 'bad key'}"},
		{403, "text/plain", "  denied ", "Parallel search failed: denied"},
		{404, "text/plain", "", "Parallel search failed: Error code: 404"},
		{200, "application/json", "nope", "Expecting value: line 1 column 1 (char 0)"},
		{200, "text/html", "<html>", "Parallel search failed: 'str' object has no attribute 'results'"},
		{200, "application/json", "null", "Parallel search failed: 'NoneType' object has no attribute 'results'"},
		{200, "application/json", `{"search_id":"s"}`, "Parallel search failed: 'SearchResult' object has no attribute 'results'"},
		{200, "application/json", `{"results":[{"title":"t"}]}`, "Parallel search failed: 'WebSearchResult' object has no attribute 'url'"},
	}
	for _, c := range cases {
		status, ctype, answer = c.status, c.ctype, c.answer
		resp, _ := parallelP().Search(context.Background(), env, "q", 5)
		if resp.Success || resp.Error != c.want {
			t.Fatalf("%d %q -> %q want %q", c.status, c.answer, resp.Error, c.want)
		}
	}
	status, ctype, answer = 200, "application/json", `{"extract_id":"e","results":[]}`
	pages, _ := parallelP().Extract(context.Background(), env, []string{"https://a"}, "")
	if pages[0].Error != "Parallel extract failed: 'ExtractResponse' object has no attribute 'errors'" {
		t.Fatalf("%+v", pages[0])
	}
}

func TestParallelConnectionFailuresAfterRetries(t *testing.T) {
	env := sdkProviderEnv(t, "PARALLEL_API_KEY=par-key\n", map[string]any{"web.keyless_fallback": false})
	sleeps := recordParallelSleeps(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close()
	t.Setenv("PARALLEL_BASE_URL", dead)
	resp, _ := parallelP().Search(context.Background(), env, "q", 5)
	if resp.Error != "Parallel search failed: Connection error." || len(*sleeps) != 2 {
		t.Fatalf("%q sleeps %v", resp.Error, *sleeps)
	}
	prev := parallelTimeouts
	parallelTimeouts = sdkTimeouts{connect: 5 * time.Second, idle: 50 * time.Millisecond}
	t.Cleanup(func() { parallelTimeouts = prev })
	slow, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond)
		writeVendorJSON(w, 200, `{"search_id":"s","results":[]}`)
	})
	t.Setenv("PARALLEL_BASE_URL", slow.URL)
	resetSDKClientSlots(t)
	resp, _ = parallelP().Search(context.Background(), env, "q", 5)
	if resp.Error != "Parallel search failed: Request timed out." {
		t.Fatalf("%q", resp.Error)
	}
}

func TestParallelCustomHeadersAndBaseURLFromTheEnvironment(t *testing.T) {
	env := sdkProviderEnv(t, "PARALLEL_API_KEY=par-key\n", map[string]any{"web.keyless_fallback": false})
	srv, calls := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
		writeVendorJSON(w, 200, `{"search_id":"s","results":[]}`)
	})
	t.Setenv("PARALLEL_BASE_URL", srv.URL+"/proxy")
	t.Setenv("PARALLEL_CUSTOM_HEADERS", "X-Team: blue\nbroken line\n X-Trace : abc ")
	parallelP().Search(context.Background(), env, "q", 5)
	c := (*calls)[0]
	if c.path != "/proxy/v1beta/search" || c.header.Get("X-Team") != "blue" || c.header.Get("X-Trace") != "abc" {
		t.Fatalf("%+v", c)
	}
}

func TestParallelClientConfig(t *testing.T) {
	env := sdkProviderEnv(t, "", nil)
	if _, err := getParallelSyncClient(context.Background(), env); err == nil || !strings.Contains(err.Error(), "PARALLEL_API_KEY") {
		t.Fatalf("no key %v", err)
	}
	t.Setenv("PARALLEL_API_KEY", "test-key")
	c1, err := getParallelSyncClient(context.Background(), env)
	if err != nil || c1.apiKey != "test-key" || c1.async {
		t.Fatalf("%+v %v", c1, err)
	}
	if c2, _ := getParallelSyncClient(context.Background(), env); c2 != c1 {
		t.Fatal("singleton rebuilt")
	}
	if a, _ := getParallelAsyncClient(context.Background(), env); a == nil || !a.async || a == c1 {
		t.Fatalf("async %+v", a)
	}
	os.Unsetenv("PARALLEL_API_KEY")
	envPath := filepath.Join(env.Home, ".env")
	for _, key := range []string{"typo-key", "fixed-key"} {
		os.WriteFile(envPath, []byte("PARALLEL_API_KEY="+key+"\n"), 0o600)
		if c, _ := getParallelSyncClient(context.Background(), env); c.apiKey != key {
			t.Fatalf("reload %q", c.apiKey)
		}
	}
	os.WriteFile(envPath, nil, 0o600)
	if _, err := getParallelSyncClient(context.Background(), env); err == nil || err.Error() != parallelMissingKey {
		t.Fatalf("removed key %v", err)
	}
}

func TestSDKProviderRouting(t *testing.T) {
	t.Run("parallel keyless when no key", func(t *testing.T) {
		env := sdkProviderEnv(t, "", map[string]any{"web.backend": "parallel"})
		swapKeyless(t, "parallel", func(context.Context, string, int) *WebSearchResponse { return searchOK(nil) }, nil)
		if resp, _ := parallelP().Search(context.Background(), env, "q", 3); !resp.Success {
			t.Fatalf("%+v", resp)
		}
	})
	t.Run("exa keyless when no key", func(t *testing.T) {
		env := sdkProviderEnv(t, "", map[string]any{"web.backend": "exa"})
		swapKeyless(t, "exa", func(context.Context, string, int) *WebSearchResponse { return searchOK(nil) }, nil)
		if resp, _ := exaP().Search(context.Background(), env, "q", 3); !resp.Success {
			t.Fatalf("%+v", resp)
		}
	})
	t.Run("parallel keyed skips keyless", func(t *testing.T) {
		env := sdkProviderEnv(t, "PARALLEL_API_KEY=sk-real\n", nil)
		srv, _ := fakeVendor(t, func(call vendorCall, w http.ResponseWriter) {
			writeVendorJSON(w, 200, `{"search_id":"s","results":[]}`)
		})
		t.Setenv("PARALLEL_BASE_URL", srv.URL)
		called := false
		swapKeyless(t, "parallel", func(context.Context, string, int) *WebSearchResponse { called = true; return searchOK(nil) }, nil)
		if resp, _ := parallelP().Search(context.Background(), env, "q", 5); !resp.Success || called {
			t.Fatalf("%+v keyless %v", resp, called)
		}
	})
	t.Run("keyless disabled falls through to key error", func(t *testing.T) {
		env := sdkProviderEnv(t, "", map[string]any{"web.keyless_fallback": false})
		if resp, _ := parallelP().Search(context.Background(), env, "q", 5); resp.Success || !strings.Contains(resp.Error, "PARALLEL_API_KEY") {
			t.Fatalf("%+v", resp)
		}
	})
	t.Run("availability stays keyed", func(t *testing.T) {
		env := sdkProviderEnv(t, "", nil)
		if parallelP().Available(env) || exaP().Available(env) || !parallelP().KeylessAvailable(env) || !exaP().KeylessAvailable(env) {
			t.Fatal("keyless leaked into availability")
		}
		t.Setenv("EXA_API_KEY", "k")
		if !exaP().Available(env) {
			t.Fatal("keyed exa unavailable")
		}
	})
	t.Run("tier free forces keyless with a key", func(t *testing.T) {
		env := sdkProviderEnv(t, "PARALLEL_API_KEY=sk-real\n", map[string]any{"web.provider_tier": map[string]any{"parallel": "free"}})
		called := 0
		swapKeyless(t, "parallel", func(context.Context, string, int) *WebSearchResponse { called++; return searchOK(nil) }, nil)
		if resp, _ := parallelP().Search(context.Background(), env, "q", 5); !resp.Success || called != 1 {
			t.Fatalf("%+v %d", resp, called)
		}
	})
	t.Run("tier paid forces keyed without key", func(t *testing.T) {
		env := sdkProviderEnv(t, "", map[string]any{"web.provider_tier": map[string]any{"parallel": "paid", "exa": "paid"}})
		called := false
		swapKeyless(t, "parallel", func(context.Context, string, int) *WebSearchResponse { called = true; return searchOK(nil) }, nil)
		if resp, _ := parallelP().Search(context.Background(), env, "q", 5); resp.Success || called || !strings.Contains(resp.Error, "PARALLEL_API_KEY") {
			t.Fatalf("%+v %v", resp, called)
		}
		if parallelP().KeylessAvailable(env) || exaP().KeylessAvailable(env) {
			t.Fatal("paid tier still keyless")
		}
	})
	t.Run("parallel keyless extract", func(t *testing.T) {
		env := sdkProviderEnv(t, "", map[string]any{"web.backend": "parallel"})
		swapKeyless(t, "parallel", nil, func(context.Context, []string) ([]*WebPage, error) {
			return []*WebPage{{URL: "https://a", Content: "c"}}, nil
		})
		if out, _ := parallelP().Extract(context.Background(), env, []string{"https://a"}, ""); out[0].Content != "c" {
			t.Fatalf("%+v", out)
		}
	})
}

// Registry holding only the exa and parallel providers for the test
func sdkOnlyRegistry(t *testing.T) {
	saved := webRegistry.ListProviders(context.Background(), "")
	ResetWebRegistryForTests()
	for _, p := range saved {
		if p.Name() == "exa" || p.Name() == "parallel" {
			RegisterWebProvider(p)
		}
	}
	t.Cleanup(func() {
		ResetWebRegistryForTests()
		for _, p := range saved {
			RegisterWebProvider(p)
		}
	})
}

func TestSDKProviderResolutionOrder(t *testing.T) {
	sdkOnlyRegistry(t)
	env := sdkProviderEnv(t, "", nil)
	p := activeSearchProvider(env)
	var want string
	for _, v := range keylessPreference() {
		if v == "exa" || v == "parallel" {
			want = v
			break
		}
	}
	if p == nil || p.Name() != want {
		t.Fatalf("keyless fallback %v want %s", p, want)
	}
	env.Config.Set("web.keyless_fallback", false)
	if p := activeSearchProvider(env); p != nil {
		t.Fatalf("keyless disabled %v", p.Name())
	}
	env.Config.Set("web.keyless_fallback", true)
	t.Setenv("EXA_API_KEY", "sk-real")
	if p := activeSearchProvider(env); p == nil || p.Name() != "exa" {
		t.Fatalf("keyed %v", p)
	}
}

func TestCheckWebAPIKeyLightsForExaOrParallelKey(t *testing.T) {
	for _, key := range []string{"PARALLEL_API_KEY", "EXA_API_KEY"} {
		env := sdkProviderEnv(t, "", map[string]any{"web.keyless_fallback": false})
		for _, k := range []string{"FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "TAVILY_API_KEY", "KEENABLE_API_KEY", "PERPLEXITY_API_KEY", "SEARXNG_URL", "BRAVE_SEARCH_API_KEY"} {
			t.Setenv(k, "")
		}
		t.Setenv(key, "test-key")
		if !CheckWebAPIKey(env) {
			t.Fatalf("%s did not light the gate", key)
		}
	}
}

func TestSDKProviderSetupSchemas(t *testing.T) {
	exa := exaP().SetupSchema()
	if exa["name"] != "Exa · Free (keyless)" || exa["web_tier"] != "free" {
		t.Fatalf("%v", exa)
	}
	paid := exa["variants"].([]any)[0].(map[string]any)
	if paid["name"] != "Exa · Paid (API key)" || paid["env_vars"].([]any)[0].(map[string]any)["key"] != "EXA_API_KEY" {
		t.Fatalf("%v", paid)
	}
	par := parallelP().SetupSchema()
	if par["name"] != "Parallel · Free (keyless)" || par["variants"].([]any)[0].(map[string]any)["env_vars"].([]any)[0].(map[string]any)["url"] != "https://parallel.ai" {
		t.Fatalf("%v", par)
	}
}

func TestSDKProvidersHonourInterrupts(t *testing.T) {
	env := sdkProviderEnv(t, "EXA_API_KEY=k\nPARALLEL_API_KEY=k\n", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if resp, _ := exaP().Search(ctx, env, "q", 5); resp.Error != "Interrupted" {
		t.Fatalf("%+v", resp)
	}
	if out, _ := parallelP().Extract(ctx, env, []string{"https://a"}, ""); out[0].Error != "Interrupted" {
		t.Fatalf("%+v", out)
	}
}

func TestProviderEnvScopeRules(t *testing.T) {
	env := sdkProviderEnv(t, "EXA_API_KEY=from-dotenv\n", nil)
	prevP := ProfileSecrets
	ProfileSecrets = ctxProfileSecrets{}
	t.Cleanup(func() { ProfileSecrets = prevP; SetMultiplexActive(false) })
	if v, err := providerEnv(context.Background(), env, "EXA_API_KEY"); v != "from-dotenv" || err != nil {
		t.Fatalf("%q %v", v, err)
	}
	scoped := context.WithValue(context.Background(), scopeKey{}, map[string]string{"EXA_API_KEY": " scoped "})
	if v, _ := providerEnv(scoped, env, "EXA_API_KEY"); v != "scoped" {
		t.Fatalf("%q", v)
	}
	empty := context.WithValue(context.Background(), scopeKey{}, map[string]string{})
	t.Setenv("PARALLEL_API_KEY", "launch")
	if v, _ := providerEnv(empty, env, "PARALLEL_API_KEY"); v != "" {
		t.Fatalf("scope miss read the process env %q", v)
	}
	if v, _ := providerEnv(empty, env, "EXA_API_KEY"); v != "from-dotenv" {
		t.Fatalf("routed scope miss skipped the profile .env %q", v)
	}
	t.Setenv("ICEBATH_HOME", env.Home)
	if v, _ := providerEnv(empty, env, "PARALLEL_API_KEY"); v != "launch" {
		t.Fatalf("launch profile scope miss skipped the process env %q", v)
	}
	if v, _ := providerEnv(empty, env, "EXA_API_KEY"); v != "from-dotenv" {
		t.Fatalf("launch profile scope miss skipped the .env %q", v)
	}
	SetMultiplexActive(true)
	if _, err := providerEnv(context.Background(), &Env{}, "EXA_API_KEY"); err != ErrUnscopedSecret {
		t.Fatalf("%v", err)
	}
}

func TestWebBackendSelectionForExaAndParallel(t *testing.T) {
	cases := []struct {
		backend string
		keys    map[string]string
		want    string
	}{
		{"parallel", nil, "parallel"},
		{"", map[string]string{"PARALLEL_API_KEY": "test-key"}, "parallel"},
		{"", map[string]string{"EXA_API_KEY": "exa-test"}, "exa"},
		{"", map[string]string{"EXA_API_KEY": "exa-test", "PARALLEL_API_KEY": "par-test"}, "exa"},
		{"", map[string]string{"EXA_API_KEY": "exa-test", "FIRECRAWL_API_KEY": "fc-test"}, "exa"},
		{"", map[string]string{"PARALLEL_API_KEY": "test-key", "FIRECRAWL_API_KEY": "fc-test"}, "parallel"},
	}
	for _, c := range cases {
		settings := map[string]any{}
		if c.backend != "" {
			settings["web.backend"] = c.backend
		}
		env := sdkProviderEnv(t, "", settings)
		for _, k := range []string{"FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "FIRECRAWL_GATEWAY_URL", "TOOL_GATEWAY_DOMAIN", "TOOL_GATEWAY_USER_TOKEN", "KEENABLE_API_KEY", "TAVILY_API_KEY", "PERPLEXITY_API_KEY"} {
			t.Setenv(k, "")
		}
		for k, v := range c.keys {
			t.Setenv(k, v)
		}
		if got := getWebBackend(env); got != c.want {
			t.Fatalf("%v %v -> %q want %q", c.backend, c.keys, got, c.want)
		}
	}
}

func TestActiveSearchProviderAndGetSearchBackendExportTheResolvers(t *testing.T) {
	sdkOnlyRegistry(t)
	env := sdkProviderEnv(t, "", nil)
	t.Setenv("EXA_API_KEY", "sk-real")
	got, want := ActiveSearchProvider(env), activeSearchProvider(env)
	if got == nil || want == nil || got.Name() != want.Name() {
		t.Fatalf("exported provider %v want %v", got, want)
	}
	if GetSearchBackend(env) != getSearchBackend(env) {
		t.Fatalf("exported backend %q want %q", GetSearchBackend(env), getSearchBackend(env))
	}
}
