package mcp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Server rejecting Streamable HTTP POSTs on /mcp through reject while GET /mcp speaks the legacy SSE transport
type legacySSE struct {
	*httptest.Server
	gets, posts, lists atomic.Int64
	mu                 sync.Mutex
	streams            map[string]chan []byte
}

func legacySSEServer(t *testing.T, reject http.HandlerFunc) *legacySSE {
	t.Helper()
	return startLegacySSE(t, reject, nil)
}

// startLegacySSE serves the legacy SSE transport, over TLS with tlsConf when it is non nil
func startLegacySSE(t *testing.T, reject http.HandlerFunc, tlsConf *tls.Config) *legacySSE {
	t.Helper()
	l := &legacySSE{streams: map[string]chan []byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			l.posts.Add(1)
			reject(w, r)
			return
		}
		sid := fmt.Sprint(l.gets.Add(1))
		out := make(chan []byte, 16)
		l.mu.Lock()
		l.streams[sid] = out
		l.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: /messages?sid=%s\n\n", sid)
		w.(http.Flusher).Flush()
		for {
			select {
			case msg := <-out:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		w.WriteHeader(http.StatusAccepted)
		if len(msg.ID) == 0 {
			return
		}
		var result any = map[string]any{}
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "legacy", "version": "1"}}
		case "tools/list":
			l.lists.Add(1)
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		l.mu.Lock()
		out := l.streams[r.URL.Query().Get("sid")]
		l.mu.Unlock()
		out <- raw
	})
	l.Server = httptest.NewUnstartedServer(mux)
	if tlsConf != nil {
		l.Server.TLS = tlsConf
		l.Server.StartTLS()
	} else {
		l.Server.Start()
	}
	t.Cleanup(l.Close)
	return l
}

// Status only rejection with an optional content type and body
func rejectWith(status int, contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// Opaque JSON-RPC internal error answered with a 2xx, so nothing is recorded as a rejection
func opaqueInternalError(w http.ResponseWriter, r *http.Request) {
	var msg struct {
		ID json.RawMessage `json:"id"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &msg)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
		"error": map[string]any{"code": -32603, "message": "Server returned an error response"}})
}

// Runs one runHTTP pass and requests shutdown once its session is ready
func serveHTTPOnce(t *testing.T, s *serverTask, r *Rendered) (string, error) {
	t.Helper()
	s.ready.Clear()
	s.shutdownEv.Clear()
	type outcome struct {
		reason string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		reason, err := s.runHTTP(context.Background(), r)
		done <- outcome{reason, err}
	}()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case o := <-done:
			return o.reason, o.err
		case <-deadline:
			t.Fatal("runHTTP never finished")
		case <-time.After(5 * time.Millisecond):
			if s.ready.IsSet() {
				s.shutdownEv.Set()
			}
		}
	}
}

// Remote task with an isolated home, its config bound for runHTTP
func httpTask(t *testing.T, name string, cfg map[string]any) (*serverTask, *Rendered) {
	t.Helper()
	s := newServerTask(name, &tools.Env{Home: os.Getenv("ICEBATH_HOME")})
	r := &Rendered{Config: cfg}
	s.config = cfg
	mcpMu.Lock()
	mcpServers[ServerKey{Name: name}] = s
	mcpMu.Unlock()
	return s, r
}

func TestServerSSEOnlyServerConnectsViaFallback(t *testing.T) {
	cases := map[string]http.HandlerFunc{"400": rejectWith(400, "", ""), "405": rejectWith(405, "", ""), "opaque": opaqueInternalError}
	for label, reject := range cases {
		t.Run(label, func(t *testing.T) {
			isolateHome(t)
			resetLedgers(t)
			fastServerTimings(t)
			startTestLoop(t)
			srv := legacySSEServer(t, reject)
			s, r := httpTask(t, "t", map[string]any{"url": srv.URL + "/mcp", "connect_timeout": 2})
			if reason, err := serveHTTPOnce(t, s, r); reason != "shutdown" || err != nil {
				t.Fatalf("fallback connect: %q %v", reason, err)
			}
			if srv.posts.Load() != 1 || srv.gets.Load() != 1 || !s.sseFallback {
				t.Fatalf("expected HTTP then SSE with a latch: posts=%d gets=%d latched=%v", srv.posts.Load(), srv.gets.Load(), s.sseFallback)
			}
			if reason, err := serveHTTPOnce(t, s, r); reason != "shutdown" || err != nil {
				t.Fatalf("latched reconnect: %q %v", reason, err)
			}
			if srv.posts.Load() != 1 || srv.gets.Load() != 2 || srv.lists.Load() != 2 {
				t.Fatalf("latched reconnect retried Streamable HTTP: posts=%d gets=%d lists=%d", srv.posts.Load(), srv.gets.Load(), srv.lists.Load())
			}
		})
	}
}

func TestServerNoSSEFallbackOnReconnectTimeoutOrServerError(t *testing.T) {
	hang := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		<-r.Context().Done()
	}
	cases := []struct {
		label  string
		reject http.HandlerFunc
		ever   bool
		check  func(error) bool
	}{
		{"proven 400", rejectWith(400, "", ""), true, func(err error) bool {
			var hse *HTTPStatusError
			return errors.As(err, &hse) && hse.Status == 400
		}},
		{"timeout", hang, false, isTimeout},
		{"500", rejectWith(500, "", ""), false, func(err error) bool {
			var hse *HTTPStatusError
			return errors.As(err, &hse) && hse.Status == 500
		}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			isolateHome(t)
			resetLedgers(t)
			srv := legacySSEServer(t, c.reject)
			s, r := httpTask(t, "t", map[string]any{"url": srv.URL + "/mcp", "connect_timeout": 0.5})
			s.everConnected = c.ever
			_, err := s.runHTTP(context.Background(), r)
			if err == nil || !c.check(err) {
				t.Fatalf("err %T %v", err, err)
			}
			if srv.gets.Load() != 0 || s.sseFallback {
				t.Fatalf("fell back to SSE: gets=%d latched=%v", srv.gets.Load(), s.sseFallback)
			}
		})
	}
}

func TestServerOpaqueRejectionReportsStatusAndBody(t *testing.T) {
	isolateHome(t)
	resetLedgers(t)
	body := `{"jsonrpc":"2.0","error":{"code":-32020,"message":"Unsupported MCP-Protocol-Version"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			rejectWith(400, "text/plain; charset=utf-8", body)(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	url := srv.URL + "/mcp"
	s, r := httpTask(t, "t", map[string]any{"url": url, "connect_timeout": 2})
	logs := captureLogs(t)
	_, err := s.runHTTP(context.Background(), r)
	detail := "Server returned an error response (HTTP 400 from POST " + url + ": " + body + ")"
	if err == nil || !strings.Contains(err.Error(), detail) || !strings.Contains(err.Error(), "; SSE: ") {
		t.Fatalf("both transports error lost the rejection: %v", err)
	}
	if logs.count(detail) != 1 {
		t.Fatalf("fallback warning lost the rejection: %s", logs.text())
	}
	if got := s.httpRejection; got.Status != 400 || got.Method != http.MethodPost || got.URL != url || got.Body != body {
		t.Fatalf("recorded rejection %+v", got)
	}
	opaque := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			opaqueInternalError(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer opaque.Close()
	s2, r2 := httpTask(t, "t2", map[string]any{"url": opaque.URL + "/mcp", "connect_timeout": 2})
	_, err = s2.runHTTP(context.Background(), r2)
	bare := "Streamable HTTP: " + (&RPCError{Code: -32603, Message: "Server returned an error response"}).Error() + "; SSE"
	if err == nil || !strings.Contains(err.Error(), bare) || s2.httpRejection != (HTTPRejection{}) {
		t.Fatalf("a 2xx answer must record nothing and add no detail: %v %+v", err, s2.httpRejection)
	}
}

func TestServerOpaqueRejectionWithoutFallbackSurfacesStatus(t *testing.T) {
	isolateHome(t)
	resetLedgers(t)
	srv := legacySSEServer(t, rejectWith(503, "text/plain", "upstream down"))
	url := srv.URL + "/mcp"
	s, r := httpTask(t, "t", map[string]any{"url": url, "connect_timeout": 2})
	s.everConnected = true
	_, err := s.runHTTP(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "HTTP 503 from POST "+url+": upstream down") {
		t.Fatalf("err %v", err)
	}
	if srv.gets.Load() != 0 {
		t.Fatal("a proven server fell back to SSE")
	}
}

// Serves status, content type and body to every method, HEAD without a body
func contentTypeServer(t *testing.T, contentType string, body string, methods *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if methods != nil {
			*methods = append(*methods, r.Method)
		}
		mu.Unlock()
		_, _ = io.ReadAll(r.Body)
		if contentType == "" {
			w.Header()["Content-Type"] = nil
		} else {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServerPreflightValidAndMissingContentTypesPass(t *testing.T) {
	isolateHome(t)
	s := newServerTask("probe_srv", nil)
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "text/event-stream", "TEXT/EVENT-STREAM", ""} {
		body := "{}"
		if ct == "" {
			body = ""
		}
		srv := contentTypeServer(t, ct, body, nil)
		if err := s.preflightContentType(context.Background(), srv.URL+"/mcp", nil, true, nil, nil, 5*time.Second, false); err != nil {
			t.Fatalf("%q must pass: %v", ct, err)
		}
	}
}

func TestServerPreflightOAuthHTMLRaisesWithoutSkip(t *testing.T) {
	isolateHome(t)
	s := newServerTask("hospitable", nil)
	srv := contentTypeServer(t, "text/html; charset=UTF-8", "<html>x</html>", nil)
	err := s.preflightContentType(context.Background(), srv.URL+"/mcp", nil, true, nil, nil, 5*time.Second, false)
	var nonMCP *NonMCPEndpointError
	if !errors.As(err, &nonMCP) || !strings.Contains(err.Error(), "hospitable") {
		t.Fatalf("err %T %v", err, err)
	}
}

// Runner task whose transport ends at once with a shutdown, so only the preflight can reach the server
func shutdownAtTransport(t *testing.T, name string, cfg map[string]any) (*serverTask, *atomic.Int64) {
	t.Helper()
	return runnerTask(t, name, cfg, func(s *serverTask, ctx context.Context, call int) (string, error) {
		s.shutdownEv.Set()
		return "shutdown", nil
	})
}

func TestServerRunSkipsPreflightForOAuthAndSkipFlag(t *testing.T) {
	isolateHome(t)
	resetLedgers(t)
	fastServerTimings(t)
	startTestLoop(t)
	var methods []string
	srv := contentTypeServer(t, "text/html; charset=UTF-8", "<html>x</html>", &methods)
	for name, extra := range map[string]map[string]any{"hospitable-test": {"auth": "oauth"}, "skip-preflight-test": {"skip_preflight": true}} {
		cfg := map[string]any{"url": srv.URL + "/mcp"}
		for k, v := range extra {
			cfg[k] = v
		}
		s, calls := shutdownAtTransport(t, name, cfg)
		_ = s.start(context.Background(), s.rendered)
		waitUntil(t, "run loop never ended", func() bool { return taskDone(s) })
		if calls.Load() != 1 || len(methods) != 0 {
			t.Fatalf("%s: transport calls %d, preflight requests %v", name, calls.Load(), methods)
		}
	}
	s, calls := shutdownAtTransport(t, "guarded", map[string]any{"url": srv.URL + "/mcp"})
	err := s.start(context.Background(), s.rendered)
	var nonMCP *NonMCPEndpointError
	if !errors.As(err, &nonMCP) || calls.Load() != 0 || len(methods) == 0 {
		t.Fatalf("an unguarded HTML endpoint must fail the preflight: %v calls=%d methods=%v", err, calls.Load(), methods)
	}
}

// Origin answering every method with a 302 to target and a target recording each request's headers
func redirectPair(t *testing.T) (origin string, seen func() []http.Header) {
	t.Helper()
	var mu sync.Mutex
	var headers []http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(target.Close)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/mcp")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(src.Close)
	return src.URL, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return append([]http.Header(nil), headers...)
	}
}

// Fails when any recorded request carried the configured credentials
func assertNoCredentials(t *testing.T, seen []http.Header) {
	t.Helper()
	if len(seen) == 0 {
		t.Fatal("the redirect target was never reached")
	}
	for _, h := range seen {
		if h.Get("Authorization") != "" || h.Get("X-Api-Key") != "" {
			t.Fatalf("credentials crossed the redirect: %v", h)
		}
		for _, vs := range h {
			for _, v := range vs {
				if strings.Contains(v, "s3cr3t") || strings.Contains(v, "k3y") {
					t.Fatalf("credential value crossed the redirect: %v", h)
				}
			}
		}
	}
}

func TestServerRunStrictRedirectHeadersReachesPreflight(t *testing.T) {
	isolateHome(t)
	resetLedgers(t)
	fastServerTimings(t)
	startTestLoop(t)
	origin, seen := redirectPair(t)
	s, _ := shutdownAtTransport(t, "strict-preflight-test", map[string]any{"url": origin + "/mcp",
		"headers": map[string]any{"Authorization": "Bearer s3cr3t", "X-API-Key": "k3y"}, "strict_redirect_headers": true})
	_ = s.start(context.Background(), s.rendered)
	waitUntil(t, "run loop never ended", func() bool { return taskDone(s) })
	assertNoCredentials(t, seen())
}

func TestServerStreamableHTTPStripsCredentialsOnRedirect(t *testing.T) {
	isolateHome(t)
	origin, seen := redirectPair(t)
	tr, err := StartStreamableHTTP(HTTPOptions{URL: origin + "/mcp", Headers: map[string]string{"Authorization": "Bearer s3cr3t", "X-API-Key": "k3y"},
		ConfiguredHeaderNames: map[string]bool{"x-api-key": true}, StrictRedirectHeaders: true, SSLVerify: true,
		ConnectTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, ServerName: "probe_srv"})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	_ = tr.Send(context.Background(), &Message{JSONRPC: "2.0", Method: "notifications/initialized"})
	assertNoCredentials(t, seen())
}

func TestServerIdentityHeaderAttachedOverHTTP(t *testing.T) {
	cases := []struct {
		label string
		cfg   map[string]any
		want  []string
	}{
		{"configured", map[string]any{"identity_header": map[string]any{"name": "X-User-Id", "value": "alice"}}, []string{"alice"}},
		{"absent", map[string]any{}, nil},
		{"explicit wins", map[string]any{"headers": map[string]any{"x-user-id": "explicit-wins"},
			"identity_header": map[string]any{"name": "X-User-Id", "value": "alice"}}, []string{"explicit-wins"}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			isolateHome(t)
			resetLedgers(t)
			fastServerTimings(t)
			startTestLoop(t)
			var posts atomic.Int64
			inner := jsonMCPServer(t, &posts)
			var mu sync.Mutex
			var initHeaders http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				if initHeaders == nil && r.Method == http.MethodPost {
					initHeaders = r.Header.Clone()
				}
				mu.Unlock()
				inner.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			cfg := map[string]any{"url": srv.URL + "/mcp"}
			for k, v := range c.cfg {
				cfg[k] = v
			}
			s, r := httpTask(t, "remote", cfg)
			if reason, err := serveHTTPOnce(t, s, r); reason != "shutdown" || err != nil {
				t.Fatalf("connect %q %v", reason, err)
			}
			mu.Lock()
			got := initHeaders.Values("X-User-Id")
			mu.Unlock()
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("X-User-Id values %v, want %v", got, c.want)
			}
		})
	}
}
