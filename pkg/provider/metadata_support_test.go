package provider

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/home"
)

// Recorded probe request
type probeCall struct {
	Method  string
	URL     string
	Headers http.Header
	Body    string
}

// Fake transport answering canned responses and recording every probe
type fakeTransport struct {
	mu     sync.Mutex
	routes map[string]func(*http.Request) *http.Response
	handle func(*http.Request) (*http.Response, error)
	calls  []probeCall
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{routes: map[string]func(*http.Request) *http.Response{}}
}

func jsonResponse(status int, body any) *http.Response {
	raw, _ := json.Marshal(body)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)), ContentLength: int64(len(raw))}
}

func textResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

// Answers the url with a fixed status and json body
func (f *fakeTransport) on(method, target string, status int, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+target] = func(*http.Request) *http.Response { return jsonResponse(status, body) }
}

func (f *fakeTransport) onFunc(method, target string, fn func(*http.Request) *http.Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+target] = fn
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
	}
	f.mu.Lock()
	f.calls = append(f.calls, probeCall{Method: req.Method, URL: req.URL.String(), Headers: req.Header.Clone(), Body: body})
	route := f.routes[req.Method+" "+req.URL.String()]
	handle := f.handle
	f.mu.Unlock()
	if route != nil {
		resp := route(req)
		resp.Request = req
		return resp, nil
	}
	if handle != nil {
		resp, err := handle(req)
		if resp != nil {
			resp.Request = req
		}
		return resp, err
	}
	return nil, errors.New("connection refused")
}

func (f *fakeTransport) urls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.URL)
	}
	return out
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeTransport) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// Clears every in process metadata memo
func resetMetadataCaches() {
	memoLock.Lock()
	modelMetadataCache = nil
	modelMetadataCacheTime = time.Time{}
	endpointModelMetadataCache = map[endpointMemoKey]map[string]map[string]any{}
	endpointModelMetadataCacheTime = map[endpointMemoKey]time.Time{}
	codexOAuthContextCache = map[string]codexContextCacheEntry{}
	codexOAuthMaxContextCache = map[string]map[string]int{}
	memoLock.Unlock()
	endpointProbePathMu.Lock()
	endpointProbePathCache = map[string]serverTypeVerdict{}
	endpointProbePathMu.Unlock()
	endpointBlackholeMu.Lock()
	endpointBlackholeCache = map[string]time.Time{}
	endpointBlackholeMu.Unlock()
	localCtxProbeMu.Lock()
	localCtxProbeCache = map[localProbeKey]localProbeHit{}
	localCtxProbeMu.Unlock()
	bedrockProbeFailureMu.Lock()
	bedrockProbeFailureCache = map[bedrockProbeKey]time.Time{}
	bedrockProbeFailureMu.Unlock()
	fallbackWarnedMu.Lock()
	fallbackWarned = map[[2]string]bool{}
	fallbackWarnedMu.Unlock()
	seenGenerationMu.Lock()
	seenGenerationIDs = map[string]bool{}
	seenGenerationMu.Unlock()
	msgTokensCache.mu.Lock()
	msgTokensCache.order = list.New()
	msgTokensCache.index = map[string]*list.Element{}
	msgTokensCache.retained = 0
	msgTokensCache.mu.Unlock()
}

// Isolated home, fake transport and clean memos for one test
func metaTest(t *testing.T) (context.Context, *fakeTransport, string) {
	t.Helper()
	dir := t.TempDir()
	ctx := home.WithOverride(context.Background(), dir)
	ft := newFakeTransport()
	metadataTransport = ft
	resetMetadataCaches()
	prevLog := metadataLog
	metadataLog = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() {
		metadataTransport = nil
		metadataLog = prevLog
		wallNow = time.Now
		monotonicNow = time.Now
		probeTotalDeadline = 15 * time.Second
		contextCacheLockTimeout = 5 * time.Second
		resetMetadataCaches()
	})
	return ctx, ft, dir
}

// Captures metadata log lines for one test
func captureMetadataLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := metadataLog
	metadataLog = slog.New(slog.NewTextHandler(buf, nil))
	t.Cleanup(func() { metadataLog = prev })
	return buf
}

// Marks the server root with a known server type verdict so detection is skipped
func seedServerType(baseURL, serverType string) {
	server := probeServerRoot(localhostToIPv4(normalizeBaseURL(baseURL)))
	endpointProbePathMu.Lock()
	endpointProbePathCache[server] = serverTypeVerdict{serverType: serverType, known: serverType != "", at: monotonicNow()}
	endpointProbePathMu.Unlock()
}

// Seeds the OpenRouter catalog memo as a fresh fetch would
func seedOpenRouterCatalog(catalog map[string]map[string]any) {
	memoLock.Lock()
	modelMetadataCache = catalog
	modelMetadataCacheTime = wallNow()
	memoLock.Unlock()
}

func writeContextCacheFile(t *testing.T, dir string, lengths map[string]any) {
	t.Helper()
	raw, err := yaml.Marshal(map[string]any{"context_lengths": lengths})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "context_length_cache.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readContextCacheFile(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "context_length_cache.yaml"))
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	lengths, _ := doc["context_lengths"].(map[string]any)
	return lengths
}

// JWT shaped token whose signature is never verified
func codexJWT(subject string) string {
	enc := func(raw []byte) string { return strings.TrimRight(base64.URLEncoding.EncodeToString(raw), "=") }
	payload, _ := json.Marshal(map[string]any{"sub": subject})
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".test-signature"
}

func codexRequest(model string) ContextLengthRequest {
	return ContextLengthRequest{Model: model, BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: codexJWT("fake-token"), Provider: "openai-codex"}
}

const codexCatalogURL = "https://chatgpt.com/backend-api/codex/models?client_version=99.0.0"

func mustEqual[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v want %v", what, got, want)
	}
}
