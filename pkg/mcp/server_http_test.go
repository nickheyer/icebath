package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Streamable HTTP MCP server answering initialize, tools/list and ping with JSON bodies
func jsonMCPServer(t *testing.T, posts *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(jsonMCPHandler(posts))
	t.Cleanup(srv.Close)
	return srv
}

// jsonMCPHandler answers Streamable HTTP initialize and tools/list with one echo tool as plain JSON
func jsonMCPHandler(posts *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		posts.Add(1)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": LatestHandshakeVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "mock", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	})
}

func TestServerPreflightContentType(t *testing.T) {
	isolateHome(t)
	s := newServerTask("probe", nil)
	ctx := context.Background()
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer html.Close()
	err := s.preflightContentType(ctx, html.URL, nil, true, nil, nil, 5*time.Second, false)
	if err == nil || !strings.Contains(err.Error(), "returned Content-Type 'text/html', not an MCP response") {
		t.Fatalf("HTML endpoint: %v", err)
	}
	var methods []string
	headThenGet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer headThenGet.Close()
	if err := s.preflightContentType(ctx, headThenGet.URL, nil, true, nil, nil, 5*time.Second, false); err != nil || strings.Join(methods, ",") != "HEAD,GET" {
		t.Fatalf("HEAD 405 then GET: %v %v", err, methods)
	}
	uiOnGet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"icebath-probe"`) || r.Header.Get("Accept") != "application/json, text/event-stream" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			return
		}
		w.Header().Set("Content-Type", "text/html")
	}))
	defer uiOnGet.Close()
	if err := s.preflightContentType(ctx, uiOnGet.URL, map[string]string{"X-Key": "v"}, true, nil, nil, 5*time.Second, false); err != nil {
		t.Fatalf("UI on GET but MCP on POST must pass: %v", err)
	}
	if err := s.preflightContentType(ctx, "http://127.0.0.1:1/mcp", nil, true, nil, nil, time.Second, false); err != nil {
		t.Fatalf("connect failure must leave the verdict to the handshake: %v", err)
	}
}

func TestServerHTTPEndToEndConnectsAndShutsDown(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	var posts atomic.Int64
	srv := jsonMCPServer(t, &posts)
	s := newServerTask("remote", &tools.Env{Home: os.Getenv("ICEBATH_HOME")})
	r := &Rendered{Config: map[string]any{"url": srv.URL, "headers": map[string]any{"X-Team": "a"}}}
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "remote"}] = s
	mcpMu.Unlock()
	if err := s.start(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if names := s.registeredToolNames(); len(names) != 1 || s.currentSession() == nil {
		t.Fatalf("registered %v", names)
	}
	s.mu.Lock()
	identity := s.resolvedIdentity
	s.mu.Unlock()
	if len(identity) != 64 || strings.Trim(identity, "0123456789abcdef") != "" {
		t.Fatalf("resolved identity %q", identity)
	}
	if err := s.keepaliveProbe(context.Background()); err != nil {
		t.Fatalf("keepalive ping: %v", err)
	}
	old := s.currentSession()
	if !signalReconnectAndWait("remote", s, "test", 5*time.Second) || s.currentSession() == old {
		t.Fatal("reconnect did not bring up a fresh session")
	}
	s.shutdown(context.Background())
	if !taskDone(s) || s.currentSession() != nil {
		t.Fatal("shutdown left the task or session")
	}
}

func TestServerSSEFallbackBothFailResetsLatch(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()
	s := newServerTask("legacy", nil)
	r := &Rendered{Config: map[string]any{"url": srv.URL, "connect_timeout": 2}}
	s.config = r.Config
	logs := captureLogs(t)
	_, err := s.runHTTP(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "both Streamable HTTP and SSE transports failed") {
		t.Fatalf("err %v", err)
	}
	var ce *mcpConnectionError
	if !errors.As(err, &ce) || s.sseFallback {
		t.Fatalf("connection error type or latch: %T latched=%v", err, s.sseFallback)
	}
	if logs.count("Streamable HTTP rejected the initial connect", "retrying over SSE") != 1 {
		t.Fatalf("fallback warning missing: %s", logs.text())
	}
	r.Config["strict_redirect_headers"] = true
	_, err = s.runHTTP(context.Background(), r)
	if err == nil || strings.Contains(err.Error(), "SSE") && strings.Contains(err.Error(), "both") {
		t.Fatalf("strict headers must not fall back to SSE: %v", err)
	}
}

func TestServerSSEStrictRedirectHeadersFailsClosed(t *testing.T) {
	isolateHome(t)
	s := newServerTask("sse", nil)
	r := &Rendered{Config: map[string]any{"url": "http://127.0.0.1:1/sse", "transport": "sse", "strict_redirect_headers": true}}
	s.config = r.Config
	_, err := s.runHTTP(context.Background(), r)
	if err == nil || err.Error() != "MCP server 'sse': strict_redirect_headers is not supported on the SSE transport." {
		t.Fatalf("err %v", err)
	}
}
