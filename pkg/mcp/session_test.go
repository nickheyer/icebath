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

// Sampler and elicitor answering every request with fixed results
type cannedHandlers struct{}

func (cannedHandlers) Capabilities() map[string]any { return map[string]any{"tools": map[string]any{}} }
func (cannedHandlers) CreateMessage(ctx context.Context, p *CreateMessageParams) (any, *RPCError) {
	return map[string]any{"role": "assistant", "model": "m", "content": map[string]any{"type": "text", "text": "sampled"}}, nil
}
func (cannedHandlers) Elicit(ctx context.Context, p *ElicitParams) (*ElicitResult, *RPCError) {
	return &ElicitResult{Action: "decline"}, nil
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
	t.Run("test_dead_loop_closes_passed_coroutine", func(t *testing.T) {
		invoked := false
		if _, err := runOnLoop(context.Background(), time.Second, func(context.Context) (int, error) { invoked = true; return 1, nil }); err == nil || err.Error() != "MCP event loop is not running" || invoked {
			t.Fatalf("dead loop %v invoked %v", err, invoked)
		}
	})
	ensureMCPLoop()
	t.Run("test_scheduler_failure_closes_factory_coroutine", func(t *testing.T) {
		l := runningLoop()
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			l.closed = false
			l.mu.Unlock()
		}()
		invoked := false
		if _, err := runOnLoop(context.Background(), time.Second, func(context.Context) (int, error) { invoked = true; return 1, nil }); err == nil || err.Error() != "MCP event loop unavailable (failed to schedule)" || invoked {
			t.Fatalf("scheduler down %v invoked %v", err, invoked)
		}
	})
	t.Run("test_interrupt_cancels_waiting_mcp_call", func(t *testing.T) {
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
	})
	t.Run("test_timeout_reports_elapsed_and_configured_timeout", func(t *testing.T) {
		cancelled := make(chan struct{})
		_, err := runOnLoop(context.Background(), 200*time.Millisecond, func(c context.Context) (int, error) {
			<-c.Done()
			close(cancelled)
			return 0, c.Err()
		})
		var te *LoopTimeoutError
		if !errors.As(err, &te) || !strings.Contains(err.Error(), "(configured timeout: 0.2s)") || !strings.HasPrefix(err.Error(), "MCP call timed out after 0.") {
			t.Fatalf("timeout %v", err)
		}
		<-cancelled
	})
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
	t.Run("test_strips_credentials", func(t *testing.T) {
		for _, c := range [][2]string{
			{"Error with ghp_abc123def456", "Error with [REDACTED]"}, {"key sk-projABC123xyz", "key [REDACTED]"},
			{"key sk-sp-ABCDEFGH12345678.abcdefgh_XYZ-0987.", "key [REDACTED]."}, {"Authorization: Bearer eyJabc123def", "Authorization: [REDACTED]"},
			{"url?token=secret123", "url?[REDACTED]"}} {
			if got := sanitizeError(c[0]); got != c[1] {
				t.Fatalf("%q sanitized %q", c[0], got)
			}
		}
		multi := sanitizeError("ghp_abc123 and sk-projXyz789 and token=foo")
		if strings.Contains(multi, "ghp_") || strings.Contains(multi, "sk-") || strings.Contains(multi, "token=") || strings.Count(multi, "[REDACTED]") != 3 {
			t.Fatalf("several credentials %q", multi)
		}
	})
	t.Run("test_no_credentials_unchanged", func(t *testing.T) {
		if got := sanitizeError("normal error message"); got != "normal error message" {
			t.Fatalf("plain message changed %q", got)
		}
	})
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
