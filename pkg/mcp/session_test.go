package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Duck typed session driving negotiation with canned outcomes
type fakeHandshake struct {
	init, disc, complete func() (*InitializeResult, error)
	calls                []string
	hang                 bool
}

func (f *fakeHandshake) Initialize(ctx context.Context) (*InitializeResult, error) {
	f.calls = append(f.calls, "initialize")
	if f.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.init()
}

func (f *fakeHandshake) CompleteHandshakeAtOfferedVersion(ctx context.Context) (*InitializeResult, error) {
	f.calls = append(f.calls, "complete")
	return f.complete()
}

// Fake that also speaks server/discover
type fakeDiscover struct{ fakeHandshake }

func (f *fakeDiscover) Discover(ctx context.Context) (*InitializeResult, error) {
	f.calls = append(f.calls, "discover")
	return f.disc()
}

// Canned outcome returning a result named by its server version
func ok(version string) func() (*InitializeResult, error) {
	return func() (*InitializeResult, error) { return &InitializeResult{ProtocolVersion: version}, nil }
}

// Canned outcome failing with a JSON-RPC error
func rpcFail(code int, msg string) func() (*InitializeResult, error) {
	return func() (*InitializeResult, error) { return nil, &RPCError{Code: code, Message: msg} }
}

func TestNegotiationModes(t *testing.T) {
	captureLogs(t)
	ctx := context.Background()
	cases := []struct {
		name      string
		mode      string
		s         handshaker
		want      string
		wantErr   string
		wantCalls string
	}{
		{"handshake first", "auto", &fakeDiscover{fakeHandshake{init: ok("INIT")}}, "INIT", "", "initialize"},
		{"-32022 falls back", "auto", &fakeDiscover{fakeHandshake{init: rpcFail(-32022, "err"), disc: ok("DISC")}}, "DISC", "", "initialize,discover"},
		{"-32601 falls back", "auto", &fakeDiscover{fakeHandshake{init: rpcFail(-32601, "Method not found: initialize"), disc: ok("DISC")}}, "DISC", "", "initialize,discover"},
		{"unrelated propagates", "auto", &fakeDiscover{fakeHandshake{init: rpcFail(-32000, "borked")}}, "", "borked", "initialize"},
		{"stateless discovers first", "stateless", &fakeDiscover{fakeHandshake{init: ok("INIT"), disc: ok("DISC")}}, "DISC", "", "discover"},
		{"stateless falls back", "stateless", &fakeDiscover{fakeHandshake{init: ok("INIT"), disc: rpcFail(-32601, "x")}}, "INIT", "", "discover,initialize"},
		{"legacy never discovers", "legacy", &fakeDiscover{fakeHandshake{init: rpcFail(-32022, "x"), disc: ok("DISC")}}, "", "x", "initialize"},
		{"unknown mode is auto", "bogus", &fakeDiscover{fakeHandshake{init: ok("INIT")}}, "INIT", "", "initialize"},
		{"no discover client reraises", "auto", &fakeHandshake{init: rpcFail(-32022, "x")}, "", "x", "initialize"},
		{"echoed modern version completes at offered", "auto", &fakeDiscover{fakeHandshake{
			init:     func() (*InitializeResult, error) { return nil, &UnsupportedServerVersionError{Version: "2026-07-28"} },
			disc:     rpcFail(-32603, "Server returned an error response"),
			complete: ok("2026-07-28")}}, "2026-07-28", "", "initialize,discover,complete"},
		{"genuine -32022 never rehandshakes", "auto", &fakeDiscover{fakeHandshake{init: rpcFail(-32022, "unsupported protocol version"),
			disc: rpcFail(-32603, "Server returned an error response")}}, "", "Server returned an error response", "initialize,discover"},
	}
	for _, c := range cases {
		res, err := negotiateSession(ctx, "negotest", c.mode, c.s, 5*time.Second)
		var calls []string
		switch f := c.s.(type) {
		case *fakeDiscover:
			calls = f.calls
		case *fakeHandshake:
			calls = f.calls
		}
		if strings.Join(calls, ",") != c.wantCalls {
			t.Fatalf("%s: calls %v", c.name, calls)
		}
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("%s: err %v", c.name, err)
			}
			continue
		}
		if err != nil || res.ProtocolVersion != c.want {
			t.Fatalf("%s: %v %v", c.name, res, err)
		}
	}
	hang := &fakeDiscover{fakeHandshake{hang: true, disc: ok("DISC")}}
	if _, err := negotiateSession(ctx, "negotest", "auto", hang, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || len(hang.calls) != 1 {
		t.Fatalf("timeout %v %v", err, hang.calls)
	}
}

func TestModernRejectionClassifier(t *testing.T) {
	if !handshakeRejectedAsModern(&RPCError{Code: -32022}) || !handshakeRejectedAsModern(&RPCError{Code: -32601}) || handshakeRejectedAsModern(&RPCError{Code: -32000, Message: "err"}) {
		t.Fatal("structural codes")
	}
	if !handshakeRejectedAsModern(errors.New("Unsupported protocol version")) || !handshakeRejectedAsModern(errors.New("Unknown method: initialize")) ||
		handshakeRejectedAsModern(errors.New("connection reset by peer")) {
		t.Fatal("substring fallbacks")
	}
	if !isMethodNotFoundError(errors.New("not found: ping")) || !handshakeAnsweredWithUnsupportedVersion(&UnsupportedServerVersionError{Version: "x"}) {
		t.Fatal("method not found markers")
	}
}

// Live session over the mock server with extra env and a record file
func mockSession(t *testing.T, extra map[string]string, opts SessionOptions) (*Session, string) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "record")
	env := map[string]string{"PATH": os.Getenv("PATH"), "MOCK_MCP_RECORD": record, "OWNED_MARKER": "ONLY_A"}
	for k, v := range extra {
		env[k] = v
	}
	tr, err := StartStdio(context.Background(), StdioParams{Command: mockBinary(t), Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Name == "" {
		opts.Name = "mock"
	}
	s := NewSession(tr, opts)
	t.Cleanup(func() { s.Close() })
	return s, record
}

// Methods the mock server received, waiting briefly for a wanted line
func recorded(t *testing.T, path, want string) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		data, _ := os.ReadFile(path)
		if want == "" || strings.Contains(string(data), want) {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	return string(data)
}

func TestSessionHandshakeAndPagination(t *testing.T) {
	logs := captureLogs(t)
	s, rec := mockSession(t, map[string]string{"MOCK_MCP_PAGES": "3"}, SessionOptions{})
	ctx := context.Background()
	res, err := s.Negotiate(ctx, "auto", 10*time.Second)
	if err != nil || res.ServerInfo.Name != "mock" || s.ProtocolVersion() != LatestHandshakeVersion || !res.Capabilities.Has("tools") {
		t.Fatalf("handshake %+v %v", res, err)
	}
	if !strings.Contains(recorded(t, rec, "notifications/initialized"), "initialize\nnotifications/initialized") {
		t.Fatalf("record %q", recorded(t, rec, ""))
	}
	hints := &CacheHints{}
	toolsList, err := s.ListTools(ctx, hints)
	if err != nil || len(toolsList) != 3 || toolsList[2].Name != "tool2" || hints.TTLMs == nil || *hints.TTLMs != 60000 || hints.CacheScope != "user" {
		t.Fatalf("pages %v %+v %v", toolsList, hints, err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	forever, _ := mockSession(t, map[string]string{"MOCK_MCP_FOREVER": "1"}, SessionOptions{Name: "loop"})
	if _, err := forever.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := forever.ListTools(ctx, nil)
	if err != nil || len(items) != listMaxPages || logs.count("tools pagination exceeded 50 pages; truncating at 50 items") != 1 {
		t.Fatalf("runaway cursor %d %v %q", len(items), err, logs.text())
	}
}

func TestSessionVersionNegotiationWithTheServer(t *testing.T) {
	captureLogs(t)
	ctx := context.Background()
	echo, rec := mockSession(t, map[string]string{"MOCK_MCP_VERSION": "2026-07-28"}, SessionOptions{})
	res, err := echo.Negotiate(ctx, "auto", 10*time.Second)
	if err != nil || res.ProtocolVersion != "2026-07-28" || echo.ProtocolVersion() != LatestHandshakeVersion || echo.InitializeResult().ProtocolVersion != LatestHandshakeVersion {
		t.Fatalf("echoed version %+v %v", res, err)
	}
	if got := recorded(t, rec, "notifications/initialized"); strings.Count(got, "initialize\n") != 2 || !strings.Contains(got, "server/discover") {
		t.Fatalf("record %q", got)
	}
	modern, _ := mockSession(t, map[string]string{"MOCK_MCP_INIT_ERROR": "-32022", "MOCK_MCP_INIT_MESSAGE": "Unsupported protocol version", "MOCK_MCP_DISCOVER": "1"}, SessionOptions{})
	res, err = modern.Negotiate(ctx, "auto", 10*time.Second)
	if err != nil || res.ServerInfo.Name != "stateless" || modern.ProtocolVersion() != "2026-07-28" {
		t.Fatalf("discover %+v %v", res, err)
	}
	if _, err := (func() (*InitializeResult, error) {
		s, _ := mockSession(t, map[string]string{"MOCK_MCP_VERSION": "1999-01-01"}, SessionOptions{})
		return s.Initialize(ctx)
	})(); err == nil || err.Error() != "Unsupported protocol version from the server: 1999-01-01" {
		t.Fatalf("unsupported %v", err)
	}
}

// Sampler and elicitor answering every request with fixed results
type cannedHandlers struct{}

func (cannedHandlers) Capabilities() map[string]any { return map[string]any{"tools": map[string]any{}} }
func (cannedHandlers) CreateMessage(ctx context.Context, p *CreateMessageParams) (any, *RPCError) {
	return map[string]any{"role": "assistant", "model": "m", "content": map[string]any{"type": "text", "text": "sampled"}}, nil
}
func (cannedHandlers) Elicit(ctx context.Context, p *ElicitParams) (*ElicitResult, *RPCError) {
	return &ElicitResult{Action: "decline"}, nil
}

func TestServerRequestsAndNotifications(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	var changed sync.WaitGroup
	changed.Add(1)
	s, _ := mockSession(t, nil, SessionOptions{Sampler: cannedHandlers{}, Elicitor: cannedHandlers{}, OnToolsListChanged: changed.Done})
	if _, err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	ask := func(method string) string {
		r, err := s.CallTool(ctx, "ask", map[string]any{"method": method}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r.Content[0].Text
	}
	if got := ask("ping"); got != `{"error":null,"result":{}}` {
		t.Fatalf("ping %s", got)
	}
	if got := ask("sampling/createMessage"); !strings.Contains(got, `"text":"sampled"`) {
		t.Fatalf("sampling %s", got)
	}
	if got := ask("elicitation/create"); !strings.Contains(got, `"action":"decline"`) {
		t.Fatalf("elicit %s", got)
	}
	for _, m := range []string{"roots/list", "bogus/method"} {
		if got := ask(m); !strings.Contains(got, `"code":-32601`) || !strings.Contains(got, "Method not found") {
			t.Fatalf("%s %s", m, got)
		}
	}
	if _, err := s.CallTool(ctx, "changed", nil, nil); err != nil {
		t.Fatal(err)
	}
	changed.Wait()
	if _, err := s.CallTool(ctx, "log", nil, nil); err != nil {
		t.Fatal(err)
	}
	if logs.count(`MCP server log [mock/http]: {"a": "rate limited", "b": 1}`) != 1 || logs.count("received tools/list_changed notification") != 1 {
		t.Fatalf("logs %q", logs.text())
	}
	bare, _ := mockSession(t, nil, SessionOptions{})
	bare.Initialize(ctx)
	r, _ := bare.CallTool(ctx, "ask", map[string]any{"method": "sampling/createMessage"}, nil)
	if !strings.Contains(r.Content[0].Text, "Sampling not supported") {
		t.Fatalf("no sampler %s", r.Content[0].Text)
	}
}

func TestServerLogCallback(t *testing.T) {
	logs := captureLogs(t)
	s := &Session{opts: SessionOptions{Name: "log_srv"}}
	s.onServerLog([]byte(`{"level":"info","data":"server started"}`))
	s.onServerLog([]byte(`{"level":"emergency","data":"` + strings.Repeat("x", 2100) + `"}`))
	s.onServerLog([]byte(`not json`))
	s.onServerLog([]byte(`{}`))
	all := logs.all()
	if all[0].msg != "MCP server log [log_srv]: server started" || all[1].msg != "MCP server log [log_srv]: "+strings.Repeat("x", 2000)+"... [truncated]" || all[1].level.String() != "ERROR" {
		t.Fatalf("records %v", all[:2])
	}
	if all[3].msg != "MCP server log [log_srv]: null" {
		t.Fatalf("missing params %v", all)
	}
	for _, lvl := range []string{"debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"} {
		if _, ok := logLevelMap[lvl]; !ok {
			t.Fatalf("%s unmapped", lvl)
		}
	}
}

func TestToolProgressReachesTheHost(t *testing.T) {
	var mu sync.Mutex
	var events []tools.Event
	env := &tools.Env{Notify: func(e tools.Event) { mu.Lock(); events = append(events, e); mu.Unlock() }}
	s, rec := mockSession(t, nil, SessionOptions{Env: env})
	ctx := context.Background()
	s.Initialize(ctx)
	r, err := s.CallTool(ctx, "progress", nil, newProgressReporter(env, "mcp__prog__slow", "call-7"))
	if err != nil || r.Content[0].Text != "done" {
		t.Fatalf("call %v %v", r, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 || len(events) >= 20 {
		t.Fatalf("events %d", len(events))
	}
	last := events[len(events)-1]
	if last.Kind != "tool.progress" || last.Tool != "mcp__prog__slow" || last.Data["tool_call_id"] != "call-7" || last.Data["percent"] != 100 || last.Text != "stage 20" {
		t.Fatalf("last %+v", last)
	}
	s.CallTool(ctx, "plain", nil, nil)
	if got := recorded(t, rec, "tools/call plain"); !strings.Contains(got, "tools/call progress progress=true") || !strings.Contains(got, "tools/call plain progress=false") {
		t.Fatalf("progress token opt in %q", got)
	}
}

func TestProgressThrottlePerCall(t *testing.T) {
	clock := 0.0
	prev := progressNow
	progressNow = func() float64 { return 100 + clock/8 }
	t.Cleanup(func() { progressNow = prev })
	var got []float64
	env := &tools.Env{Notify: func(e tools.Event) { got = append(got, e.Data["progress"].(float64)) }}
	report := newProgressReporter(env, "t", "call-1")
	for step := 1; step <= 10; step++ {
		report(float64(step*10), fptr(100), "")
		clock++
	}
	if len(got) != 4 || got[0] != 10 || got[1] != 50 || got[2] != 90 || got[3] != 100 {
		t.Fatalf("throttled %v", got)
	}
}

func TestPendingRequestsFailWhenTheTransportCloses(t *testing.T) {
	s, rec := mockSession(t, nil, SessionOptions{})
	ctx := context.Background()
	s.Initialize(ctx)
	callCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := s.CallTool(callCtx, "hang", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled call %v", err)
	}
	if !strings.Contains(recorded(t, rec, "notifications/cancelled"), "notifications/cancelled") {
		t.Fatal("server never told about the cancelled request")
	}
	errc := make(chan error, 1)
	go func() {
		_, err := s.CallTool(ctx, "hang", nil, nil)
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	s.Close()
	if err := <-errc; RPCCode(err) != codeConnectionClosed || !strings.Contains(err.Error(), "Connection closed") {
		t.Fatalf("pending after close %v", err)
	}
}

func TestLoopHomeOverridePropagation(t *testing.T) {
	isolateHome(t)
	processHome := t.TempDir()
	t.Setenv("ICEBATH_HOME", processHome)
	ensureMCPLoop()
	t.Cleanup(func() { stopMCPLoop(false) })
	readHome := func(ctx context.Context) (string, error) { return home.Dir(ctx), nil }
	if got, _ := runOnLoop(context.Background(), 10*time.Second, readHome); got != processHome {
		t.Fatalf("unscoped %q", got)
	}
	profile := t.TempDir()
	if got, _ := runOnLoop(home.WithOverride(context.Background(), profile), 10*time.Second, readHome); got != profile {
		t.Fatalf("scoped %q", got)
	}
	homes := []string{t.TempDir(), t.TempDir()}
	results := make([]string, 2)
	var wg sync.WaitGroup
	for i := range homes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = runOnLoop(home.WithOverride(context.Background(), homes[i]), 10*time.Second, readHome)
		}(i)
	}
	wg.Wait()
	if results[0] != homes[0] || results[1] != homes[1] {
		t.Fatalf("concurrent scopes %v", results)
	}
	if got, _ := runOnLoop(context.Background(), 10*time.Second, readHome); got != processHome {
		t.Fatalf("after scopes %q", got)
	}
	task := context.Background()
	if wrapWithHomeOverride(context.Background(), task) != task || wrapWithDashboardOAuthFlow(context.Background(), task) != task {
		t.Fatal("wrapping without an override must hand back the task context")
	}
}

func TestLoopInterruptTimeoutAndStop(t *testing.T) {
	isolateHome(t)
	if _, err := runOnLoop(context.Background(), time.Second, func(context.Context) (int, error) { return 1, nil }); err == nil || err.Error() != "MCP event loop is not running" {
		t.Fatalf("dead loop %v", err)
	}
	ensureMCPLoop()
	cancelled := make(chan struct{})
	worker := tools.NewWorkerID()
	ctx := tools.WithWorker(context.Background(), worker)
	go func() {
		time.Sleep(150 * time.Millisecond)
		tools.SetInterrupt(worker, true, "")
	}()
	_, err := runOnLoop(ctx, 10*time.Second, func(c context.Context) (string, error) {
		<-c.Done()
		close(cancelled)
		return "", c.Err()
	})
	tools.SetInterrupt(worker, false, "")
	if !errors.Is(err, ErrInterrupted) || err.Error() != "User sent a new message" {
		t.Fatalf("interrupt %v", err)
	}
	<-cancelled
	_, err = runOnLoop(context.Background(), 200*time.Millisecond, func(c context.Context) (int, error) {
		<-c.Done()
		return 0, c.Err()
	})
	var te *LoopTimeoutError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "(configured timeout: 0.2s)") || !strings.HasPrefix(err.Error(), "MCP call timed out after 0.") {
		t.Fatalf("timeout %v", err)
	}
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "live"}] = nil
	mcpMu.Unlock()
	if stopMCPLoop(true) {
		t.Fatal("idle only stop ran with a registered server")
	}
	mcpMu.Lock()
	delete(mcpServers, ServerKey{Name: "live"})
	mcpMu.Unlock()
	stuck := make(chan struct{})
	if err := spawnOnLoop(context.Background(), func(c context.Context) { <-c.Done(); close(stuck) }); err != nil {
		t.Fatal(err)
	}
	if !stopMCPLoop(true) || runningLoop() != nil {
		t.Fatal("stop failed")
	}
	<-stuck
	if err := spawnOnLoop(context.Background(), func(context.Context) {}); err == nil {
		t.Fatal("spawn after stop succeeded")
	}
}

func TestDiscoveryLock(t *testing.T) {
	isolateHome(t)
	discoveryLockMu.Lock()
	discoveryLockPath = ""
	discoveryLockMu.Unlock()
	ctx := context.Background()
	cookie, outcome := tryAcquireDiscoveryLock(ctx)
	if outcome != lockAcquired || cookie == nil {
		t.Fatalf("acquire %v", outcome)
	}
	if _, again := tryAcquireDiscoveryLock(ctx); again != lockHeld {
		t.Fatalf("second holder %v", again)
	}
	cookie.Release()
	cookie.Release()
	next, outcome := tryAcquireDiscoveryLock(ctx)
	if outcome != lockAcquired {
		t.Fatalf("reacquire %v", outcome)
	}
	next.Release()
	routed := t.TempDir()
	if c, o := tryAcquireDiscoveryLock(home.WithOverride(ctx, routed)); o != lockAcquired {
		t.Fatalf("routed profile %v", o)
	} else {
		if _, err := os.Stat(filepath.Join(routed, ".mcp-discovery.lock")); err != nil {
			t.Fatal("routed profile must lock under its own home")
		}
		c.Release()
	}
	gone := home.WithOverride(ctx, filepath.Join(t.TempDir(), "missing", "dir"))
	if _, o := tryAcquireDiscoveryLock(gone); o != lockUnavailable {
		t.Fatalf("unopenable path %v", o)
	}
}

// Reconnectable stand in exposing its events and session
type fakeServer struct {
	mu        sync.Mutex
	reconnect *event
	ready     *event
	session   *Session
}

func (f *fakeServer) reconnectEvent() *event { return f.reconnect }
func (f *fakeServer) readyEvent() *event     { return f.ready }
func (f *fakeServer) currentSession() *Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.session
}

func TestReconnectSignalling(t *testing.T) {
	isolateHome(t)
	prev := readinessPoll
	readinessPoll = func() { time.Sleep(20 * time.Millisecond) }
	t.Cleanup(func() { readinessPoll = prev })
	if signalReconnect(&fakeServer{}) {
		t.Fatal("no reconnect machinery must report false")
	}
	if ReconnectServer(context.Background(), "absent") {
		t.Fatal("absent server reconnected")
	}
	old := &Session{}
	srv := &fakeServer{reconnect: newEvent(), ready: newEvent(), session: old}
	srv.ready.Set()
	if signalReconnectAndWait("srv", srv, "test", time.Second) {
		t.Fatal("reconnect requested without a running loop")
	}
	ensureMCPLoop()
	t.Cleanup(func() { stopMCPLoop(false) })
	go func() {
		<-srv.reconnect.Wait()
		time.Sleep(5 * time.Millisecond)
		srv.mu.Lock()
		srv.session = &Session{}
		srv.mu.Unlock()
		srv.ready.Set()
	}()
	if !signalReconnectAndWait("srv", srv, "auth recovery", time.Second) {
		t.Fatal("fresh session never observed")
	}
	stale := &fakeServer{ready: newEvent(), session: old}
	stale.ready.Set()
	if waitForServerSessionReady(stale, old, 0) || !waitForServerSessionReady(stale, nil, 0) {
		t.Fatal("readiness poll")
	}
}

func TestScopeKeysAndRekey(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	if serverKey(ctx, "github", "", true) != (ServerKey{Name: "github"}) {
		t.Fatal("unscoped key must stay bare")
	}
	scoped := tools.WithSessionToolScope(ctx, "/profiles/b")
	b := serverKey(scoped, "github", "", true)
	if !b.Scoped() || b.Name != "github" {
		t.Fatalf("scoped key %v", b)
	}
	mcpMu.Lock()
	shared := ServerKey{Scope: "profile-a", Name: "github"}
	mcpServers[shared] = nil
	serverToolScopes[shared] = map[string]bool{b.Scope: true}
	got := resolveServerKey(scoped, "github", "", true)
	if got != shared || !keyVisibleInScope(shared, b.Scope) || keyVisibleInScope(shared, "other") || !keyVisibleInScope(shared, "") {
		mcpMu.Unlock()
		t.Fatalf("adopted key %v", got)
	}
	own := ServerKey{Scope: b.Scope, Name: "github"}
	lazyServerConfigs[own] = map[string]any{}
	if resolveServerKey(scoped, "github", "", true) != own {
		mcpMu.Unlock()
		t.Fatal("own lazy registration must win")
	}
	delete(lazyServerConfigs, own)
	delete(mcpServers, shared)
	if resolveServerKey(scoped, "github", "", true) != own {
		mcpMu.Unlock()
		t.Fatal("no live shared connection resolves to the scope's own key")
	}
	bare := ServerKey{Name: "solo"}
	mcpServers[bare] = nil
	serverScopeKeys[bare] = ""
	serverErrorCounts[bare] = 2
	parallelSafeServers[bare] = struct{}{}
	lazyServerToolNames[ServerKey{Name: "lazy"}] = []string{"mcp__lazy__t"}
	mcpMu.Unlock()
	RekeyUnscopedConnections("launch")
	mcpMu.Lock()
	defer mcpMu.Unlock()
	moved := ServerKey{Scope: "launch", Name: "solo"}
	if _, ok := mcpServers[moved]; !ok || serverScopeKeys[moved] != "launch" || serverErrorCounts[moved] != 2 || !serverToolScopes[moved]["launch"] ||
		!serverToolScopes[ServerKey{Scope: "launch", Name: "lazy"}]["launch"] {
		t.Fatalf("rekey %v %v %v", mcpServers, serverScopeKeys, serverToolScopes)
	}
	if _, ok := parallelSafeServers[moved]; !ok {
		t.Fatal("sets must rekey too")
	}
	if _, ok := mcpServers[bare]; ok {
		t.Fatal("bare key left behind")
	}
}

func TestCommonSessionHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"Error with ghp_abc123def456": "Error with [REDACTED]", "key sk-projABC123xyz": "key [REDACTED]",
		"auth Bearer eyJhbGc.x failed": "auth [REDACTED] failed", "url?token=abc&x=1": "url?[REDACTED]&x=1",
		"API_KEY=zzz and password=hunter2": "[REDACTED] and [REDACTED]", "plain failure": "plain failure",
	} {
		if got := sanitizeError(in); got != want {
			t.Fatalf("%q sanitized %q", in, got)
		}
	}
	type emptyErr struct{ error }
	if got := excStr(emptyErr{errors.New("")}); got == "" || excStr(errors.New("  ")) == "" || excStr(errors.New("real")) != "real" {
		t.Fatalf("excStr %q", got)
	}
	for i := 0; i < 100; i++ {
		if j := jittered(10); j < 8 || j > 12 {
			t.Fatalf("jitter %v", j)
		}
	}
	if jittered(-1) != 0 {
		t.Fatal("jitter must floor at zero")
	}
}
