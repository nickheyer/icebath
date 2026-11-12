package gatewayrun

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Loopback chat completions server streaming the given SSE chunks and recording the request
type proxyServer struct {
	*httptest.Server
	mu      sync.Mutex
	url     string
	headers http.Header
	body    map[string]any
	written int
}

func newProxyServer(t *testing.T, chunks []string, afterDone func(w http.ResponseWriter)) *proxyServer {
	t.Helper()
	ps := &proxyServer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		ps.mu.Lock()
		ps.url, ps.headers = r.URL.Path, r.Header.Clone()
		_ = json.Unmarshal(raw, &ps.body)
		ps.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, chunk := range chunks {
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			ps.mu.Lock()
			ps.written++
			ps.mu.Unlock()
		}
		if afterDone != nil {
			afterDone(w)
		}
	}))
	t.Cleanup(ps.Close)
	return ps
}

// Bare runner of the proxy cases with no adapters and a fresh home
func proxyRunner(t *testing.T) *GatewayRunner {
	t.Helper()
	r, _, _ := updateRunner(t)
	r.adapters = adapterDict()
	return r
}

func matrixSource() *gateway.SessionSource {
	name, userID, userName := "Test Room", "@user:server.org", "testuser"
	return &gateway.SessionSource{Platform: gateway.PlatformMatrix, ChatID: "!room:server.org", ChatName: &name, ChatType: "group",
		UserID: &userID, UserName: &userName}
}

func runProxy(t *testing.T, r *GatewayRunner, p proxyRunParams) map[string]any {
	t.Helper()
	if p.source == nil {
		p.source = matrixSource()
	}
	result, err := r.runAgentViaProxy(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestReturnsNoneWhenNotConfigured(t *testing.T) {
	t.Setenv("GATEWAY_PROXY_URL", "")
	if got := proxyRunner(t).getProxyURL(context.Background()); got != "" {
		t.Fatalf("proxy url = %q", got)
	}
}

func TestReadsFromConfigYaml(t *testing.T) {
	t.Setenv("GATEWAY_PROXY_URL", "")
	r, _, hermesHome := updateRunner(t)
	writeHomeFile(t, hermesHome, "config.yaml", []byte("gateway:\n  proxy_url: http://10.0.0.1:8642\n"))
	if got := r.getProxyURL(context.Background()); got != "http://10.0.0.1:8642" {
		t.Fatalf("proxy url = %q", got)
	}
}

func TestBuildsCorrectRequest(t *testing.T) {
	server := newProxyServer(t, []string{`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":" world"}}]}` + "\n\n" + "data: [DONE]\n\n"}, nil)
	t.Setenv("GATEWAY_PROXY_URL", server.URL)
	t.Setenv("GATEWAY_PROXY_KEY", "test-key-123")
	result := runProxy(t, proxyRunner(t), proxyRunParams{message: "How are you?", contextPrompt: "You are helpful.",
		history: []map[string]any{{"role": "user", "content": "Hello"}, {"role": "assistant", "content": "Hi there!"}}, sessionID: "session-abc"})
	if server.url != "/v1/chat/completions" {
		t.Fatalf("path = %q", server.url)
	}
	if server.headers.Get("Authorization") != "Bearer test-key-123" || server.headers.Get("X-Icebath-Session-Id") != "session-abc" {
		t.Fatalf("headers = %v", server.headers)
	}
	want := []any{
		map[string]any{"role": "system", "content": "You are helpful."}, map[string]any{"role": "user", "content": "Hello"},
		map[string]any{"role": "assistant", "content": "Hi there!"}, map[string]any{"role": "user", "content": "How are you?"},
	}
	if !reflect.DeepEqual(server.body["messages"], want) || server.body["stream"] != true {
		t.Fatalf("body = %v", server.body)
	}
	if result["final_response"] != "Hello world" {
		t.Fatalf("final = %v", result["final_response"])
	}
}

func TestHandlesConnectionError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	t.Setenv("GATEWAY_PROXY_URL", "http://"+addr)
	t.Setenv("GATEWAY_PROXY_KEY", "")
	result := runProxy(t, proxyRunner(t), proxyRunParams{message: "hi", sessionID: "test"})
	if !strings.Contains(strings.ToLower(result["final_response"].(string)), "connection refused") || result["api_calls"] != 0 {
		t.Fatalf("result = %v", result)
	}
}

func TestNoSystemMessageWhenContextEmpty(t *testing.T) {
	server := newProxyServer(t, []string{`data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\ndata: [DONE]\n\n"}, nil)
	t.Setenv("GATEWAY_PROXY_URL", server.URL)
	t.Setenv("GATEWAY_PROXY_KEY", "")
	runProxy(t, proxyRunner(t), proxyRunParams{message: "hello", sessionID: "test"})
	messages, _ := server.body["messages"].([]any)
	if len(messages) != 1 || !reflect.DeepEqual(messages[0], map[string]any{"role": "user", "content": "hello"}) {
		t.Fatalf("messages = %v", messages)
	}
}

func TestDoneMarkerStopsReadingTrailingChunks(t *testing.T) {
	trailing := make(chan bool, 1)
	server := newProxyServer(t, []string{`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n", "data: [DONE]\n"},
		func(w http.ResponseWriter) {
			time.Sleep(200 * time.Millisecond)
			_, err := w.Write([]byte(`data: {"choices":[{"delta":{"content":" IGNORED"}}]}` + "\n"))
			if err == nil {
				err = http.NewResponseController(w).Flush()
			}
			trailing <- err == nil
		})
	t.Setenv("GATEWAY_PROXY_URL", server.URL)
	t.Setenv("GATEWAY_PROXY_KEY", "")
	result := runProxy(t, proxyRunner(t), proxyRunParams{message: "hi", sessionID: "test"})
	if result["final_response"] != "Hello" {
		t.Fatalf("final = %v", result["final_response"])
	}
	server.mu.Lock()
	written := server.written
	server.mu.Unlock()
	if written != 2 {
		t.Fatalf("chunks written before the reader stopped = %d", written)
	}
	<-trailing
}

func TestResidualBufferFlushedAfterEOF(t *testing.T) {
	server := newProxyServer(t, []string{`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n",
		`data: {"choices":[{"delta":{"content":" world"}}]}`}, nil)
	t.Setenv("GATEWAY_PROXY_URL", server.URL)
	t.Setenv("GATEWAY_PROXY_KEY", "")
	result := runProxy(t, proxyRunner(t), proxyRunParams{message: "hi", sessionID: "test"})
	if result["final_response"] != "Hello world" {
		t.Fatalf("final = %v", result["final_response"])
	}
}

func TestEOFWithoutDoneAndNoContentIsAnError(t *testing.T) {
	server := newProxyServer(t, nil, nil)
	t.Setenv("GATEWAY_PROXY_URL", server.URL)
	t.Setenv("GATEWAY_PROXY_KEY", "")
	result := runProxy(t, proxyRunner(t), proxyRunParams{message: "hi", sessionID: "test"})
	if result["final_response"] == "" || result["api_calls"] != 0 {
		t.Fatalf("result = %v", result)
	}
}

func TestClientTimeoutSetsSockConnect(t *testing.T) {
	if proxySockConnect <= 0 || proxySockConnect > 60*time.Second {
		t.Fatalf("sock_connect should be a short, reasonable cap, got %s", proxySockConnect)
	}
}

func TestMalformedChunkIsSkippedNotFatal(t *testing.T) {
	server := newProxyServer(t, []string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n", `data: {"choices":[null]}` + "\n",
		`data: {"choices":"wrong-type"}` + "\n", `data: {"choices":[{"delta":"wrong-type"}]}` + "\n",
		`data: {"choices":[{"delta":{"content":" world"}}]}` + "\n", "data: [DONE]\n",
	}, nil)
	t.Setenv("GATEWAY_PROXY_URL", server.URL)
	t.Setenv("GATEWAY_PROXY_KEY", "")
	result := runProxy(t, proxyRunner(t), proxyRunParams{message: "hi", sessionID: "test"})
	if result["final_response"] != "Hello world" {
		t.Fatalf("final = %v", result["final_response"])
	}
}
