package provider

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type redirectRecordedRequest struct {
	method  string
	headers map[string]string
}

// Recording handler shared by a source and a sink server, redirecting every /redirect path
type redirectRecorder struct {
	mu             sync.Mutex
	requests       []redirectRecordedRequest
	redirectTo     string
	redirectStatus int
}

func (h *redirectRecorder) record(r *http.Request) {
	headers := map[string]string{}
	for name, values := range r.Header {
		headers[strings.ToLower(name)] = values[0]
	}
	h.mu.Lock()
	h.requests = append(h.requests, redirectRecordedRequest{method: r.Method, headers: headers})
	h.mu.Unlock()
}

func (h *redirectRecorder) redirect(w http.ResponseWriter) {
	h.mu.Lock()
	target, status := h.redirectTo, h.redirectStatus
	h.mu.Unlock()
	w.Header().Set("Location", target)
	w.WriteHeader(status)
}

func (h *redirectRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		io.ReadAll(r.Body)
		if r.URL.Path == "/redirect" {
			h.redirect(w)
			return
		}
		h.record(r)
		w.WriteHeader(http.StatusOK)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/redirect") {
		h.redirect(w)
		return
	}
	h.record(r)
	body := []byte(`{"data": []}`)
	w.Header().Set("Content-Length", "12")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (h *redirectRecorder) last(t *testing.T) redirectRecordedRequest {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.requests) == 0 {
		t.Fatal("no request reached the sink")
	}
	return h.requests[len(h.requests)-1]
}

// Source and sink servers behind one recorder redirecting to the sink under the given host name
func redirectPair(t *testing.T, sinkHost string, status int) (*redirectRecorder, *httptest.Server) {
	t.Helper()
	h := &redirectRecorder{redirectStatus: status}
	source := httptest.NewServer(h)
	sink := httptest.NewServer(h)
	t.Cleanup(source.Close)
	t.Cleanup(sink.Close)
	sinkURL, _ := url.Parse(sink.URL)
	h.redirectTo = "http://" + sinkHost + ":" + sinkURL.Port() + "/sink"
	return h, source
}

func credentialHeaders() map[string]string {
	return map[string]string{
		"Authorization":           "Bearer secret",
		"Cookie":                  "session=secret",
		"CF-Access-Client-Secret": "cloudflare-secret",
		"X-Custom-Auth":           "tenant-secret",
		"Accept":                  "application/json",
		"User-Agent":              "hermes-test",
	}
}

func requestWithHeaders(t *testing.T, method, target string, headers map[string]string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return req
}

func openAndRead(t *testing.T, req *http.Request) {
	t.Helper()
	resp, err := OpenCredentialedURL(req, 3*time.Second, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
}

func TestCrossHostRedirectDropsArbitraryCredentialsOnWire(t *testing.T) {
	h, source := redirectPair(t, "localhost", 302)
	openAndRead(t, requestWithHeaders(t, http.MethodGet, source.URL+"/redirect", credentialHeaders(), nil))
	last := h.last(t)
	if last.method != "GET" {
		t.Fatalf("method = %q", last.method)
	}
	if last.headers["accept"] != "application/json" || last.headers["user-agent"] != "hermes-test" {
		t.Fatalf("safe headers lost: %v", last.headers)
	}
	for _, name := range []string{"authorization", "cookie", "cf-access-client-secret", "x-custom-auth"} {
		if _, present := last.headers[name]; present {
			t.Fatalf("%s crossed the origin: %v", name, last.headers)
		}
	}
}

func TestSameHostDifferentPortDropsCredentialsOnWire(t *testing.T) {
	h, source := redirectPair(t, "127.0.0.1", 302)
	openAndRead(t, requestWithHeaders(t, http.MethodGet, source.URL+"/redirect", credentialHeaders(), nil))
	last := h.last(t)
	if _, present := last.headers["authorization"]; present {
		t.Fatalf("authorization crossed the port: %v", last.headers)
	}
	if _, present := last.headers["cf-access-client-secret"]; present {
		t.Fatalf("cf-access-client-secret crossed the port: %v", last.headers)
	}
}

// Redirect hop request as the client builds it, carrying the response status that caused it
func redirectHop(t *testing.T, target string, status int) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Response = &http.Response{StatusCode: status}
	return req
}

func TestPost307RemainsRejectedByUrllib(t *testing.T) {
	request := requestWithHeaders(t, http.MethodPost, "https://models.example.test/load", credentialHeaders(), strings.NewReader("{}"))
	handler, err := NewSafeCredentialRedirectHandler(request.URL.String())
	if err != nil {
		t.Fatal(err)
	}
	hop := redirectHop(t, "https://other.example.test/load", 307)
	hop.Method = http.MethodPost
	if got := handler.RedirectRequest(hop, []*http.Request{request}); got != http.ErrUseLastResponse {
		t.Fatalf("307 after POST = %v, want the response handed back unfollowed", got)
	}
}

type recordingOpener struct {
	calls *[][2]any
}

func (o recordingOpener) Open(req *http.Request, timeout time.Duration) (*http.Response, error) {
	*o.calls = append(*o.calls, [2]any{req.URL.String(), timeout})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

func TestExplicitOpenerFactoryIsInstrumentableWithoutSecurityBypass(t *testing.T) {
	var calls [][2]any
	factory := func(redirect *SafeCredentialRedirectHandler) RequestOpener {
		if redirect == nil {
			t.Fatal("factory received no redirect handler")
		}
		return recordingOpener{calls: &calls}
	}
	request := requestWithHeaders(t, http.MethodGet, "https://models.example.test/models", map[string]string{"Authorization": "secret"}, nil)
	resp, err := OpenCredentialedURL(request, 7*time.Second, factory, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(calls) != 1 || calls[0] != [2]any{"https://models.example.test/models", 7 * time.Second} {
		t.Fatalf("calls = %v", calls)
	}
}

func TestInstalledRequestProcessorCannotResurrectCrossOriginSecret(t *testing.T) {
	h, source := redirectPair(t, "localhost", 302)
	installed := &Opener{
		Processors: []RequestProcessor{func(req *http.Request) *http.Request {
			req.Header.Set("X-Installed-Secret", "must-not-cross")
			return req
		}},
		AddHeaders: [][2]string{{"X-Opener-Secret", "also-must-not-cross"}},
	}
	InstallOpener(installed)
	t.Cleanup(func() { InstallOpener(nil) })
	openAndRead(t, requestWithHeaders(t, http.MethodGet, source.URL+"/redirect", map[string]string{"Authorization": "Bearer secret"}, nil))
	last := h.last(t)
	for _, name := range []string{"authorization", "x-installed-secret", "x-opener-secret"} {
		if _, present := last.headers[name]; present {
			t.Fatalf("%s crossed the origin: %v", name, last.headers)
		}
	}
}

func TestMultihopRedirectsNeverResurrectCredentials(t *testing.T) {
	request := requestWithHeaders(t, http.MethodGet, "https://a.example.test/models", credentialHeaders(), nil)
	handler, err := NewSafeCredentialRedirectHandler(request.URL.String())
	if err != nil {
		t.Fatal(err)
	}
	sameOrigin := redirectHop(t, "https://a.example.test/step-two", 302)
	if err := handler.RedirectRequest(sameOrigin, []*http.Request{request}); err != nil {
		t.Fatal(err)
	}
	if sameOrigin.Header.Get("Authorization") == "" {
		t.Fatalf("same origin hop lost authorization: %v", sameOrigin.Header)
	}
	crossOrigin := redirectHop(t, "https://b.example.test/step-three", 302)
	if err := handler.RedirectRequest(crossOrigin, []*http.Request{request, sameOrigin}); err != nil {
		t.Fatal(err)
	}
	if crossOrigin.Header.Get("Authorization") != "" || crossOrigin.Header.Get("CF-Access-Client-Secret") != "" {
		t.Fatalf("cross origin hop kept credentials: %v", crossOrigin.Header)
	}
	returned := redirectHop(t, "https://a.example.test/final", 302)
	if err := handler.RedirectRequest(returned, []*http.Request{request, sameOrigin, crossOrigin}); err != nil {
		t.Fatal(err)
	}
	if returned.Header.Get("Authorization") != "" || returned.Header.Get("CF-Access-Client-Secret") != "" {
		t.Fatalf("returning hop resurrected credentials: %v", returned.Header)
	}
}

func TestProbeApiModelsDropsCustomCredentialsOnWire(t *testing.T) {
	h, source := redirectPair(t, "localhost", 302)
	ctx := contextWithTempHome(t)
	result := ProbeAPIModels(ctx, "provider-key", source.URL+"/redirect/..", 3*time.Second, "", map[string]string{
		"CF-Access-Client-Secret": "cloudflare-secret",
		"X-Custom-Auth":           "tenant-secret",
	})
	if len(result.Models) != 0 {
		t.Fatalf("models = %v", result.Models)
	}
	last := h.last(t)
	for _, name := range []string{"authorization", "cf-access-client-secret", "x-custom-auth"} {
		if _, present := last.headers[name]; present {
			t.Fatalf("%s crossed the origin: %v", name, last.headers)
		}
	}
}

// Round tripper sending the anthropic models URL to a local server as the Request monkeypatch does
type anthropicRewriteTransport struct {
	source *url.URL
}

func (t anthropicRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), "https://api.anthropic.com/v1/models") {
		rewritten := req.Clone(req.Context())
		rewritten.URL.Scheme = "http"
		rewritten.URL.Host = t.source.Host
		rewritten.URL.Path = "/redirect"
		rewritten.URL.RawQuery = ""
		rewritten.Host = ""
		return http.DefaultTransport.RoundTrip(rewritten)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestAnthropicProfileDropsXApiKeyOnRedirect(t *testing.T) {
	h, source := redirectPair(t, "localhost", 302)
	sourceURL, _ := url.Parse(source.URL)
	prev := modelsTransport
	modelsTransport = anthropicRewriteTransport{source: sourceURL}
	t.Cleanup(func() { modelsTransport = prev })
	profile := anthropicPluginProfiles()[0]
	ids, _ := profile.FetchModels(contextWithTempHome(t), "anthropic-secret", "", 3*time.Second)
	if len(ids) != 0 {
		t.Fatalf("models = %v", ids)
	}
	last := h.last(t)
	if _, present := last.headers["x-api-key"]; present {
		t.Fatalf("x-api-key crossed the origin: %v", last.headers)
	}
	if last.headers["accept"] != "application/json" {
		t.Fatalf("accept lost: %v", last.headers)
	}
}

func TestAzureCatalogProbeDropsApiKeyAndBearerOnRedirect(t *testing.T) {
	h, source := redirectPair(t, "localhost", 302)
	status, body := (&AzureDetector{}).httpGetJSON(t.Context(), source.URL+"/redirect", "azure-secret", nil)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	data, ok := body["data"].([]any)
	if !ok || len(data) != 0 || len(body) != 1 {
		t.Fatalf("body = %v", body)
	}
	last := h.last(t)
	if _, present := last.headers["authorization"]; present {
		t.Fatalf("authorization crossed the origin: %v", last.headers)
	}
	if _, present := last.headers["api-key"]; present {
		t.Fatalf("api-key crossed the origin: %v", last.headers)
	}
}

func TestAzureAnthropicProbeDropsApiKeyAndBearerOnRedirect(t *testing.T) {
	h := &redirectRecorder{redirectStatus: 302}
	sink := httptest.NewServer(h)
	t.Cleanup(sink.Close)
	sinkURL, _ := url.Parse(sink.URL)
	target := "http://localhost:" + sinkURL.Port() + "/sink"
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(source.Close)
	(&AzureDetector{}).probeAnthropicMessages(t.Context(), source.URL, "azure-secret", nil)
	last := h.last(t)
	if _, present := last.headers["authorization"]; present {
		t.Fatalf("authorization crossed the origin: %v", last.headers)
	}
	if _, present := last.headers["api-key"]; present {
		t.Fatalf("api-key crossed the origin: %v", last.headers)
	}
}

func swapResolvedHTTPSContext(t *testing.T, fn func() *tls.Config) {
	t.Helper()
	prev := resolvedHTTPSContext
	resolvedHTTPSContext = fn
	t.Cleanup(func() { resolvedHTTPSContext = prev })
}

// TLS context the opener's HTTPS handler is bound to
func openerHTTPSContext(t *testing.T, opener *Opener) *tls.Config {
	t.Helper()
	transport, ok := opener.roundTripper(0).(*openerTransport).inner.(*http.Transport)
	if !ok {
		t.Fatalf("opener transport %T is not an HTTPS handler", opener.Transport)
	}
	return transport.TLSClientConfig
}

func TestHermesOwnedOpenerUsesResolvedHttpsContext(t *testing.T) {
	context := &tls.Config{MinVersion: tls.VersionTLS12}
	InstallOpener(nil)
	swapResolvedHTTPSContext(t, func() *tls.Config { return context })
	opener, err := secureOpenerFromInstalledPolicy("https://models.example.test/catalog", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opener.TLS != context || openerHTTPSContext(t, opener) != context {
		t.Fatal("owned opener did not bind the resolved context")
	}
}

func TestResolvedHttpsContextDefersToThePlatformStore(t *testing.T) {
	if resolvedHTTPSContext() != nil {
		t.Fatal("resolved context is not urllib's default")
	}
	for _, name := range []string{"ICEBATH_CA_BUNDLE", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		t.Setenv(name, t.TempDir()+"/nope.pem")
	}
	if resolvedHTTPSContext() != nil {
		t.Fatal("a CA bundle variable steered the resolved context")
	}
}

func TestResolvedHttpsContextInstallsThePlatformVerifier(t *testing.T) {
	resolvedHTTPSContext()
	trustMu.Lock()
	defer trustMu.Unlock()
	if trustInstalled == nil {
		t.Fatal("resolving the context left the platform trust store uninstalled")
	}
}

func TestInstalledHttpsContextIsPreserved(t *testing.T) {
	context := &tls.Config{MinVersion: tls.VersionTLS12}
	InstallOpener(&Opener{TLS: context})
	t.Cleanup(func() { InstallOpener(nil) })
	resolved := false
	swapResolvedHTTPSContext(t, func() *tls.Config {
		resolved = true
		return nil
	})
	opener, err := secureOpenerFromInstalledPolicy("https://models.example.test/catalog", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved {
		t.Fatal("installed TLS policy must remain authoritative")
	}
	if opener.TLS != context || openerHTTPSContext(t, opener) != context {
		t.Fatal("installed context was replaced")
	}
}
