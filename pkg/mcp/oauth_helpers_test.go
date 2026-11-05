package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	b.buf.Reset()
	b.mu.Unlock()
}

var (
	testStderr   = &syncBuffer{}
	testStdinMu  sync.Mutex
	testStdinFn  func() (string, error)
	testStdinHit = make(chan struct{}, 64)
)

// Routes the stdin and stderr seams through test controlled values for the whole binary
func init() {
	oauthStderr = testStderr
	oauthStdinReadLine = func() (string, error) {
		testStdinMu.Lock()
		fn := testStdinFn
		testStdinMu.Unlock()
		select {
		case testStdinHit <- struct{}{}:
		default:
		}
		if fn == nil {
			return "", io.EOF
		}
		return fn()
	}
}

// Answers stdin reads with fn until the test ends
func fakeStdin(t *testing.T, fn func() (string, error)) {
	t.Helper()
	testStdinMu.Lock()
	testStdinFn = fn
	testStdinMu.Unlock()
	t.Cleanup(func() {
		testStdinMu.Lock()
		testStdinFn = nil
		testStdinMu.Unlock()
	})
}

// Stdin read that stays blocked until the test ends
func blockingStdin(t *testing.T) {
	t.Helper()
	release := make(chan struct{})
	fakeStdin(t, func() (string, error) {
		<-release
		return "", io.EOF
	})
	t.Cleanup(func() { close(release) })
}

// Makes stdin look like a console or not for the test
func setConsole(t *testing.T, console bool) {
	t.Helper()
	prev := stdinIsConsole
	stdinIsConsole = func() bool { return console }
	t.Cleanup(func() { stdinIsConsole = prev })
}

// Clears the reserved sockets, CIMD port claims and captured stderr around the test
func resetOAuthState(t *testing.T) {
	t.Helper()
	clear := func() {
		reservedMu.Lock()
		for port, ln := range reservedListeners {
			ln.Close()
			delete(reservedListeners, port)
		}
		reservedOrder = nil
		reservedMu.Unlock()
		assignedCIMDPorts = nil
		testStderr.Reset()
	}
	clear()
	t.Cleanup(clear)
}

// Isolated home plus clean OAuth globals, returning the home directory
func oauthHome(t *testing.T) string {
	t.Helper()
	dir := isolateHome(t)
	resetOAuthState(t)
	return dir
}

// Writes a JSON value or raw string into the token dir of a home
func writeTokenFile(t *testing.T, homeDir, name string, v any) string {
	t.Helper()
	dir := filepath.Join(homeDir, "mcp-tokens")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var data []byte
	if s, ok := v.(string); ok {
		data = []byte(s)
	} else {
		var err error
		if data, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Decoded JSON object of a file, failing the test when unreadable
func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

// Reports whether a path exists
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Polls a callback URL until the waiter's listener answers
func hitCallbackWhenReady(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("callback listener never came up: " + url)
}

// Runs a waiter in a goroutine, returning its outcome channel
func runWaiter(ctx context.Context, w func(context.Context) (*AuthorizationCodeResult, error)) chan waiterOutcome {
	ch := make(chan waiterOutcome, 1)
	go func() {
		r, err := w(ctx)
		ch <- waiterOutcome{r, err}
	}()
	return ch
}

type waiterOutcome struct {
	res *AuthorizationCodeResult
	err error
}

// Waits for a waiter outcome, failing after the timeout
func awaitWaiter(t *testing.T, ch chan waiterOutcome, timeout time.Duration) waiterOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(timeout):
		t.Fatal("waiter did not finish")
	}
	return waiterOutcome{}
}

// Test token with Bearer type
func bearer(access, refresh string) *OAuthToken {
	t := &OAuthToken{AccessToken: access, TokenType: "Bearer"}
	if refresh != "" {
		t.RefreshToken = strPtr(refresh)
	}
	return t
}

// Replaces the browser launcher for the test
func fakeBrowser(t *testing.T, fn func(string) bool) {
	t.Helper()
	prev := openBrowser
	openBrowser = fn
	t.Cleanup(func() { openBrowser = prev })
}

// Binds 127.0.0.1 on a port, the way a competing process would
func netListen4(port int) (net.Listener, error) {
	return net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
}

// Provider for a server URL over the given storage, with default client metadata when md is nil
func newTestProvider(t *testing.T, s *TokenStorage, serverURL string, md *OAuthClientMetadata) *OAuthProvider {
	t.Helper()
	if md == nil {
		var err error
		if md, err = validateClientMetadata(map[string]any{"redirect_uris": []any{"http://127.0.0.1:1/callback"},
			"client_name": "Icebath Agent", "grant_types": []any{"authorization_code", "refresh_token"}}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := newOAuthProvider(oauthProviderOptions{ServerURL: serverURL, ClientMetadata: md, Storage: s, OAuthFlow: "browser"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type recordedRequest struct {
	Method string
	URL    string
	Path   string
	Header http.Header
	Body   string
}

type oauthFakeServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
}

// HTTP server recording every request before answering it with handle
func newOAuthFakeServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body string)) *oauthFakeServer {
	t.Helper()
	fs := &oauthFakeServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		fs.mu.Lock()
		fs.reqs = append(fs.reqs, recordedRequest{r.Method, r.URL.String(), r.URL.Path, r.Header.Clone(), string(data)})
		fs.mu.Unlock()
		handle(w, r, string(data))
	}))
	t.Cleanup(fs.Close)
	return fs
}

// Requests seen so far
func (fs *oauthFakeServer) requests() []recordedRequest {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]recordedRequest(nil), fs.reqs...)
}

// Writes a JSON body with a status
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Unauthorized response carrying a WWW-Authenticate challenge for the request
func challenge401(req *http.Request, wwwAuth string) *http.Response {
	h := http.Header{}
	if wwwAuth != "" {
		h.Set("WWW-Authenticate", wwwAuth)
	}
	return &http.Response{StatusCode: http.StatusUnauthorized, Header: h, Request: req, Body: http.NoBody}
}

type roundTripFunc func(*http.Request) *http.Response

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

type standIn struct {
	mu     sync.Mutex
	hits   [][2]string
	bodies map[string]string
	route  func(r *http.Request, body string) (int, any, http.Header)
}

// Mock transport answering every request through route and recording method and URL
func newStandIn(route func(r *http.Request, body string) (int, any, http.Header)) *standIn {
	return &standIn{route: route, bodies: map[string]string{}}
}

func (s *standIn) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	s.mu.Lock()
	s.hits = append(s.hits, [2]string{r.Method, r.URL.String()})
	s.bodies[r.Method+" "+r.URL.String()] = string(body)
	s.mu.Unlock()
	status, v, h := s.route(r, string(body))
	if h == nil {
		h = http.Header{}
	}
	var data []byte
	switch b := v.(type) {
	case nil:
	case string:
		data = []byte(b)
	case []byte:
		data = b
	default:
		data, _ = json.Marshal(b)
		h.Set("Content-Type", "application/json")
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(data)), Request: r, ContentLength: int64(len(data))}, nil
}

// Reports whether a method and URL were requested
func (s *standIn) hit(method, url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hits {
		if h[0] == method && h[1] == url {
			return true
		}
	}
	return false
}

// Recorded hits
func (s *standIn) allHits() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]string(nil), s.hits...)
}

// Sends a request through the provider the way the HTTP transport does, retrying once after a handled challenge
func authedDo(ctx context.Context, p *OAuthProvider, rt http.RoundTripper, method, rawURL string) (*http.Response, error) {
	client := &http.Client{Transport: rt}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := p.Prepare(ctx, req); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	retry, err := p.HandleUnauthorized(ctx, req, resp)
	if err != nil || !retry {
		return resp, err
	}
	resp.Body.Close()
	return client.Do(req)
}

// Pointer to a bool literal
func boolPtr(b bool) *bool { return &b }
