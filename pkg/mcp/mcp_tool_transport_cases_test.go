package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// test_mcp_sse_transport.py TestSSEReadTimeout test_sse_read_timeout_is_300s_not_tool_timeout
func TestSSEReadTimeoutSseReadTimeoutIs300sNotToolTimeout(t *testing.T) {
	if sseReadTimeout != 300*time.Second {
		t.Fatalf("sse read timeout %v", sseReadTimeout)
	}
	if sseReadTimeout == 60*time.Second {
		t.Fatal("the SSE idle read timeout must not be the 60s tool timeout")
	}
}

// Authenticator double stamping a bearer token and counting its preparations
type bearerAuth struct{ prepared atomic.Int32 }

func (a *bearerAuth) Prepare(_ context.Context, req *http.Request) error {
	a.prepared.Add(1)
	req.Header.Set("Authorization", "Bearer fake-oauth-token")
	return nil
}

func (a *bearerAuth) HandleUnauthorized(context.Context, *http.Request, *http.Response) (bool, error) {
	return false, nil
}

// test_mcp_sse_transport.py TestSSEOAuthForwarding test_sse_client_receives_oauth_auth_when_configured
func TestSSEOAuthForwardingSseClientReceivesOauthAuthWhenConfigured(t *testing.T) {
	isolateHome(t)
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: endpoint\ndata: /messages?sid=1\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	auth := &bearerAuth{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr, err := StartSSE(ctx, HTTPOptions{URL: srv.URL + "/mcp", Auth: auth, ServerName: "sse-test"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Close()
	mu.Lock()
	defer mu.Unlock()
	if auth.prepared.Load() == 0 || len(seen) == 0 || seen[0] != "Bearer fake-oauth-token" {
		t.Fatalf("sse stream opened without the oauth authenticator: prepared %d headers %v", auth.prepared.Load(), seen)
	}
	plain := newServerTask("sse-test", nil)
	if got, err := plain.buildOAuthAuth(ctx, srv.URL+"/mcp", map[string]any{}, HTTPOptions{}); got != nil || err != nil {
		t.Fatalf("a server without oauth auth built %v %v", got, err)
	}
}

// Transport answering the handshake and tool listing, then dropping its stream after the session went live
type droppingTransport struct {
	in      chan *Message
	once    sync.Once
	mu      sync.Mutex
	err     error
	dropErr error
	methods []string
}

func (d *droppingTransport) Send(_ context.Context, msg *Message) error {
	d.mu.Lock()
	d.methods = append(d.methods, msg.Method)
	d.mu.Unlock()
	if !msg.IsRequest() {
		return nil
	}
	var reply any = map[string]any{}
	switch msg.Method {
	case "initialize":
		reply = InitializeResult{ProtocolVersion: LatestHandshakeVersion, Capabilities: &ServerCapabilities{Tools: map[string]any{"listChanged": false}}, ServerInfo: Implementation{Name: "fake", Version: "1"}}
	case "tools/list":
		reply = map[string]any{"tools": []any{}}
	}
	resp, err := NewResponse(msg.ID, reply)
	if err != nil {
		return err
	}
	d.in <- resp
	if msg.Method == "tools/list" {
		d.drop()
	}
	return nil
}

func (d *droppingTransport) drop() {
	d.once.Do(func() {
		d.mu.Lock()
		d.err = d.dropErr
		d.mu.Unlock()
		close(d.in)
	})
}

func (d *droppingTransport) Receive() <-chan *Message { return d.in }

func (d *droppingTransport) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *droppingTransport) Close() error {
	d.drop()
	return nil
}

func (d *droppingTransport) Label() string { return "dropping" }

// The _make_http_task helper, a remote task whose transport drops right after the live session is ready
func makeDroppingHTTPTask(t *testing.T) (*serverTask, func() (Transport, error), *droppingTransport) {
	t.Helper()
	isolateHome(t)
	resetLedgers(t)
	fastServerTimings(t)
	ensureMCPLoop()
	s := newServerTask("http-drop", nil)
	s.config = map[string]any{"url": "http://127.0.0.1:9/mcp"}
	tr := &droppingTransport{in: make(chan *Message, 16), dropErr: errors.New("stream dropped")}
	open := func() (Transport, error) { return tr, nil }
	return s, open, tr
}

// Serves the dropping transport with a bound, failing with the methods seen when the run never ends
func serveDropping(t *testing.T, s *serverTask, open func() (Transport, error), tr *droppingTransport) (string, error) {
	t.Helper()
	type outcome struct {
		reason string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		reason, err := s.serveTransport(context.Background(), open, "HTTP", 5)
		done <- outcome{reason, err}
	}()
	select {
	case o := <-done:
		return o.reason, o.err
	case <-time.After(5 * time.Second):
		tr.mu.Lock()
		defer tr.mu.Unlock()
		t.Fatalf("the run never ended after the drop; methods seen %v", tr.methods)
		return "", nil
	}
}

// test_mcp_transport_group_reconnect.py test_run_http_transport_group_returns_reconnect
func TestRunHttpTransportGroupReturnsReconnect(t *testing.T) {
	s, open, tr := makeDroppingHTTPTask(t)
	reason, err := serveDropping(t, s, open, tr)
	if err != nil || reason != "reconnect" {
		t.Fatalf("reason %q err %v", reason, err)
	}
	if !s.ready.IsSet() {
		t.Fatal("readiness must stay set for run() to clear on re-entry")
	}
}

// test_mcp_transport_group_reconnect.py test_run_http_transport_group_reraises_on_shutdown
func TestRunHttpTransportGroupReraisesOnShutdown(t *testing.T) {
	s, open, tr := makeDroppingHTTPTask(t)
	s.shutdownEv.Set()
	reason, err := serveDropping(t, s, open, tr)
	if reason == "reconnect" {
		t.Fatalf("a drop during shutdown reconnected: reason %q err %v", reason, err)
	}
	if err == nil && reason != "shutdown" {
		t.Fatalf("a drop during shutdown must end the run: reason %q", reason)
	}
}
