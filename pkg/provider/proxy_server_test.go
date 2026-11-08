package provider

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// How long the fake adapter blocks, long enough for a frozen server to record no heartbeats
const proxyStallDuration = 500 * time.Millisecond

const proxyHeartbeatInterval = 10 * time.Millisecond

const proxyMinTicksAcrossStall = 3

// Adapter recording how the server ran each blocking call, standing in for the auth store lock and token refresh
type recordingAdapter struct {
	baseURL           string
	stall             time.Duration
	ticks             *atomic.Int64
	raiseOnCredential bool
	retryBearer       string
	raiseOnRetry      bool
	entered           chan struct{}
	release           chan struct{}

	mu                         sync.Mutex
	credentialCalls            int
	authenticatedCalls         int
	retryCalls                 int
	retryStatusCode            int
	ticksAcrossCredential      int64
	ticksAcrossIsAuthenticated int64
	ticksAcrossRetry           int64
}

func (a *recordingAdapter) Name() string        { return "recording" }
func (a *recordingAdapter) DisplayName() string { return "Recording Provider" }
func (a *recordingAdapter) AllowedPaths() map[string]bool {
	return map[string]bool{"/chat/completions": true}
}

// Blocks the way the real adapters do, on the release channel or for the stall, counting heartbeats meanwhile
func (a *recordingAdapter) block() int64 {
	before := a.ticks.Load()
	if a.entered != nil {
		a.entered <- struct{}{}
	}
	if a.release != nil {
		<-a.release
	}
	if a.stall > 0 {
		time.Sleep(a.stall)
	}
	return a.ticks.Load() - before
}

func (a *recordingAdapter) IsAuthenticated() bool {
	across := a.block()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.authenticatedCalls++
	a.ticksAcrossIsAuthenticated = across
	return true
}

func (a *recordingAdapter) GetCredential() (UpstreamCredential, error) {
	across := a.block()
	a.mu.Lock()
	a.credentialCalls++
	a.ticksAcrossCredential = across
	a.mu.Unlock()
	if a.raiseOnCredential {
		return UpstreamCredential{}, errorString("simulated auth failure")
	}
	return UpstreamCredential{Bearer: "test-bearer", BaseURL: a.baseURL, ExpiresAt: "2099-01-01T00:00:00Z"}, nil
}

func (a *recordingAdapter) GetRetryCredential(_ UpstreamCredential, statusCode int) (*UpstreamCredential, error) {
	across := a.block()
	a.mu.Lock()
	a.retryCalls++
	a.retryStatusCode = statusCode
	a.ticksAcrossRetry = across
	a.mu.Unlock()
	if a.raiseOnRetry {
		return nil, errorString("simulated retry-credential failure")
	}
	if a.retryBearer == "" {
		return nil, nil
	}
	return &UpstreamCredential{Bearer: a.retryBearer, BaseURL: a.baseURL, ExpiresAt: "2099-01-01T00:00:00Z"}, nil
}

type errorString string

func (e errorString) Error() string { return string(e) }

func newRecordingAdapter(baseURL string) *recordingAdapter {
	return &recordingAdapter{baseURL: baseURL, ticks: &atomic.Int64{}}
}

// Captured upstream requests, their path and Authorization header
type capturedRequests struct {
	mu       sync.Mutex
	requests []map[string]string
}

func (c *capturedRequests) add(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, map[string]string{"path": r.URL.Path, "auth": r.Header.Get("Authorization")})
}

func (c *capturedRequests) auths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.requests))
	for _, r := range c.requests {
		out = append(out, r["auth"])
	}
	return out
}

// Upstream echoing every /v1/chat/completions request as JSON
func buildFakeUpstream(t *testing.T, captured *capturedRequests) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.add(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"echoed": true, "body": string(body)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Upstream rejecting every bearer except the accepted one with the given status
func buildRejectingUpstream(t *testing.T, captured *capturedRequests, rejectStatus int, acceptBearer string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		captured.add(r)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+acceptBearer {
			w.WriteHeader(rejectStatus)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rejected"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"echoed": true})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Proxy app on an ephemeral loopback port
func startProxy(t *testing.T, adapter UpstreamAdapter) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(createProxyApp(adapter, DefaultProxyHost, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv
}

// Ticks the adapter's heartbeat counter until the test ends
func startHeartbeat(t *testing.T, adapter *recordingAdapter) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(proxyHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				adapter.ticks.Add(1)
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

func postProxyChat(t *testing.T, proxyURL string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(proxyURL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// A request the proxy answers without the adapter, proving the server keeps serving while a call blocks
func proxyServesMeanwhile(t *testing.T, proxyURL string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(proxyURL + "/v1/not-forwarded")
	if err != nil {
		t.Fatalf("the proxy stopped serving while an adapter call blocked: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("meanwhile status %d", resp.StatusCode)
	}
}

func TestGetCredentialRunsOffTheEventLoop(t *testing.T) {
	captured := &capturedRequests{}
	upstream := buildFakeUpstream(t, captured)
	adapter := newRecordingAdapter(upstream.URL + "/v1")
	adapter.entered, adapter.release = make(chan struct{}, 1), make(chan struct{})
	proxy := startProxy(t, adapter)
	type result struct {
		status int
	}
	results := make(chan result, 1)
	go func() {
		resp, _ := postProxyChat(t, proxy.URL)
		results <- result{resp.StatusCode}
	}()
	<-adapter.entered
	proxyServesMeanwhile(t, proxy.URL)
	close(adapter.release)
	if got := <-results; got.status != http.StatusOK {
		t.Fatalf("status %d", got.status)
	}
	adapter.mu.Lock()
	calls := adapter.credentialCalls
	adapter.mu.Unlock()
	if calls == 0 {
		t.Fatal("get_credential was never called")
	}
	if auths := captured.auths(); len(auths) != 1 || auths[0] != "Bearer test-bearer" {
		t.Fatalf("forwarded auths %v", auths)
	}
}

func TestEventLoopKeepsRunningWhileCredentialsResolve(t *testing.T) {
	captured := &capturedRequests{}
	upstream := buildFakeUpstream(t, captured)
	adapter := newRecordingAdapter(upstream.URL + "/v1")
	adapter.stall = proxyStallDuration
	proxy := startProxy(t, adapter)
	startHeartbeat(t, adapter)
	postProxyChat(t, proxy.URL)
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.credentialCalls == 0 || adapter.ticksAcrossCredential < proxyMinTicksAcrossStall {
		t.Fatalf("only %d heartbeats ran during a %s credential resolution — the server was frozen", adapter.ticksAcrossCredential, proxyStallDuration)
	}
}

func TestCredentialFailureStillMapsTo401(t *testing.T) {
	captured := &capturedRequests{}
	upstream := buildFakeUpstream(t, captured)
	adapter := newRecordingAdapter(upstream.URL + "/v1")
	adapter.raiseOnCredential = true
	proxy := startProxy(t, adapter)
	resp, body := postProxyChat(t, proxy.URL)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var payload map[string]map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["error"]["code"] != "upstream_auth_failed" || !strings.Contains(payload["error"]["message"].(string), "simulated auth failure") {
		t.Fatalf("payload %v", payload)
	}
	if len(captured.auths()) != 0 {
		t.Fatal("the request reached the upstream")
	}
}

func TestGetRetryCredentialRunsOffTheEventLoop(t *testing.T) {
	captured := &capturedRequests{}
	upstream := buildRejectingUpstream(t, captured, http.StatusUnauthorized, "rotated-bearer")
	adapter := newRecordingAdapter(upstream.URL + "/v1")
	adapter.retryBearer = "rotated-bearer"
	proxy := startProxy(t, adapter)
	resp, _ := postProxyChat(t, proxy.URL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	adapter.mu.Lock()
	retryCalls, retryStatus := adapter.retryCalls, adapter.retryStatusCode
	adapter.mu.Unlock()
	if retryCalls == 0 {
		t.Fatal("get_retry_credential was never called")
	}
	if retryStatus != http.StatusUnauthorized {
		t.Fatalf("retry status %d", retryStatus)
	}
	if auths := captured.auths(); len(auths) != 2 || auths[0] != "Bearer test-bearer" || auths[1] != "Bearer rotated-bearer" {
		t.Fatalf("forwarded auths %v", auths)
	}
}

func TestEventLoopKeepsRunningWhileTheRetryCredentialResolves(t *testing.T) {
	captured := &capturedRequests{}
	upstream := buildRejectingUpstream(t, captured, http.StatusTooManyRequests, "rotated-bearer")
	adapter := newRecordingAdapter(upstream.URL + "/v1")
	adapter.stall = proxyStallDuration
	adapter.retryBearer = "rotated-bearer"
	proxy := startProxy(t, adapter)
	startHeartbeat(t, adapter)
	postProxyChat(t, proxy.URL)
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.retryCalls == 0 || adapter.ticksAcrossRetry < proxyMinTicksAcrossStall {
		t.Fatalf("only %d heartbeats ran during a %s 429 credential rotation — the server was frozen", adapter.ticksAcrossRetry, proxyStallDuration)
	}
}

func TestRetryCredentialFailureStillReturnsTheUpstreamRejection(t *testing.T) {
	captured := &capturedRequests{}
	upstream := buildRejectingUpstream(t, captured, http.StatusUnauthorized, "never-offered")
	adapter := newRecordingAdapter(upstream.URL + "/v1")
	adapter.raiseOnRetry = true
	proxy := startProxy(t, adapter)
	resp, _ := postProxyChat(t, proxy.URL)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(captured.auths()) != 1 {
		t.Fatalf("forwards %v", captured.auths())
	}
}

func TestIsAuthenticatedRunsOffTheEventLoop(t *testing.T) {
	adapter := newRecordingAdapter("http://127.0.0.1:1/v1")
	adapter.entered, adapter.release = make(chan struct{}, 1), make(chan struct{})
	proxy := startProxy(t, adapter)
	type health struct {
		status  int
		payload map[string]any
	}
	results := make(chan health, 1)
	go func() {
		resp, err := http.Get(proxy.URL + "/health")
		if err != nil {
			results <- health{}
			return
		}
		defer resp.Body.Close()
		var payload map[string]any
		json.NewDecoder(resp.Body).Decode(&payload)
		results <- health{resp.StatusCode, payload}
	}()
	<-adapter.entered
	proxyServesMeanwhile(t, proxy.URL)
	close(adapter.release)
	got := <-results
	if got.status != http.StatusOK || got.payload["authenticated"] != true {
		t.Fatalf("health %d %v", got.status, got.payload)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.authenticatedCalls == 0 {
		t.Fatal("is_authenticated was never called")
	}
}

func TestEventLoopKeepsRunningWhileHealthResolvesAuthState(t *testing.T) {
	adapter := newRecordingAdapter("http://127.0.0.1:1/v1")
	adapter.stall = proxyStallDuration
	proxy := startProxy(t, adapter)
	startHeartbeat(t, adapter)
	resp, err := http.Get(proxy.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.authenticatedCalls == 0 || adapter.ticksAcrossIsAuthenticated < proxyMinTicksAcrossStall {
		t.Fatalf("only %d heartbeats ran during a %s /health auth check — the server was frozen", adapter.ticksAcrossIsAuthenticated, proxyStallDuration)
	}
}
