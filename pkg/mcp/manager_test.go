package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fresh home with fast lifecycle timings, a running loop and every connection closed afterwards
func managerHome(t *testing.T) (string, *tools.Env) {
	t.Helper()
	dir := isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	t.Cleanup(func() {
		ShutdownMCPServers(context.Background(), nil, nil, time.Second)
		mcpMu.Lock()
		clear(mcpToolServerNames)
		clear(serverConnectFailures)
		clear(serverConnectRetryAfter)
		clear(serverErrorCounts)
		clear(serverBreakerOpenedAt)
		clear(serverErrorsAllApplication)
		mcpMu.Unlock()
	})
	return dir, &tools.Env{Home: dir}
}

func TestConnectCooldownHelpersFailureArmsExponentialBackoff(t *testing.T) {
	isolateHome(t)
	now := time.Unix(1000, 0)
	managerNow = func() time.Time { return now }
	t.Cleanup(func() { managerNow = time.Now })
	ctx := context.Background()
	want := []time.Duration{30, 60, 120, 240, 480, 600, 600, 600}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	for i, w := range want {
		recordConnectFailure(ctx, "x")
		if got := serverConnectRetryAfter[ServerKey{Name: "x"}].Sub(now); got != w*time.Second {
			t.Fatalf("failure %d backoff %v want %v", i+1, got, w*time.Second)
		}
	}
	if !connectCooldownActive(ctx, "x") || connectCooldownActive(tools.WithSessionToolScope(ctx, "/p"), "x") {
		t.Fatal("cooldown must be per scope")
	}
	clearConnectFailure(ctx, "x")
	if connectCooldownActive(ctx, "x") || serverConnectFailures[ServerKey{Name: "x"}] != 0 {
		t.Fatal("success must clear the cooldown")
	}
}

func TestManagerBreakerBumpAndReset(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	k := ServerKey{Name: "s"}
	bumpServerError(ctx, "s", true)
	bumpServerError(ctx, "s", false)
	bumpServerError(ctx, "s", true)
	mcpMu.Lock()
	n, opened, app := serverErrorCounts[k], !serverBreakerOpenedAt[k].IsZero(), serverErrorsAllApplication[k]
	mcpMu.Unlock()
	if n != 3 || !opened || app {
		t.Fatalf("count %d opened %v all application %v", n, opened, app)
	}
	resetServerError(ctx, "s")
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if serverErrorCounts[k] != 0 || !serverBreakerOpenedAt[k].IsZero() {
		t.Fatal("reset must close the breaker")
	}
	if _, ok := serverErrorsAllApplication[k]; ok {
		t.Fatal("reset must drop the application flag")
	}
}

func TestManagerRegistryScopeAndOwnerSecretScope(t *testing.T) {
	dir := isolateHome(t)
	ctx := context.Background()
	env := &tools.Env{Home: dir}
	if mcpRegistryScope(ctx) != "" || installOwnerSecretScope(ctx) != ctx || ownerEnv(ctx, env) != env {
		t.Fatal("a single profile process binds nothing")
	}
	if got := mcpRegistryScope(tools.WithSessionToolScope(ctx, "/sess")); got != "/sess" {
		t.Fatalf("session scope %q", got)
	}
	other := t.TempDir()
	routed := home.WithOverride(ctx, other)
	if got := mcpRegistryScope(routed); got != tools.ScopeKey(other) {
		t.Fatalf("routed scope %q", got)
	}
	if err := os.WriteFile(filepath.Join(other, ".env"), []byte("TOKEN=owner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scoped := installOwnerSecretScope(routed)
	if currentSecretScopeHome(scoped) != tools.ScopeKey(other) {
		t.Fatal("owner scope must be stamped with the owning profile")
	}
	oenv := ownerEnv(routed, env)
	if v, _ := oenv.Secrets.Get("TOKEN"); v != "owner" || oenv.Home != tools.ScopeKey(other) {
		t.Fatalf("owner env reads %q from %q", v, oenv.Home)
	}
}

func TestManagerOwnerHydrationRetriesOncePerInterval(t *testing.T) {
	dir := isolateHome(t)
	now := time.Unix(5000, 0)
	managerNow = func() time.Time { return now }
	t.Cleanup(func() { managerNow = time.Now })
	calls := 0
	prev := hydrateSecretSources
	hydrateSecretSources = func(string) bool { calls++; return false }
	t.Cleanup(func() { hydrateSecretSources = prev })
	ownerSecretMapping(dir)
	ownerSecretMapping(dir)
	if calls != 1 {
		t.Fatalf("hydrated %d times inside the interval", calls)
	}
	now = now.Add(hydrateRetryInterval)
	ownerSecretMapping(dir)
	if calls != 2 {
		t.Fatal("hydration must retry after the interval")
	}
}

func TestDiscoveryCrossProcessLock(t *testing.T) {
	dir := isolateHome(t)
	logs := captureLogs(t)
	ctx := home.WithOverride(context.Background(), dir)
	t.Run("test_lock_acquired_path", func(t *testing.T) {
		cookie := acquireDiscoveryLockWithRetry(ctx)
		if cookie == nil {
			t.Fatal("free lock must be acquired")
		}
		cookie.Release()
		again := acquireDiscoveryLockWithRetry(ctx)
		if again == nil {
			t.Fatal("released lock must be acquirable")
		}
		again.Release()
	})
	t.Run("test_lock_held_retries_exhausted_fallback", func(t *testing.T) {
		held := acquireDiscoveryLockWithRetry(ctx)
		if held == nil {
			t.Fatal("free lock must be acquired")
		}
		defer held.Release()
		sleeps := 0
		discoveryLockSleep = func() { sleeps++ }
		t.Cleanup(func() { discoveryLockSleep = func() { time.Sleep(discoveryLockRetryDelay) } })
		if got := acquireDiscoveryLockWithRetry(ctx); got != nil {
			t.Fatal("held lock must fall back to unguarded")
		}
		if sleeps != discoveryLockMaxRetries || logs.count("still held after 620 retries") != 1 {
			t.Fatalf("slept %d times, logs %s", sleeps, logs.text())
		}
	})
}

func TestDiscoveryConcurrencyConfig(t *testing.T) {
	isolateHome(t)
	logs := captureLogs(t)
	ctx := context.Background()
	for body, want := range map[string]int{"": 4, "mcp:\n  discovery_concurrency: 0\n": 0, "mcp:\n  discovery_concurrency: 9\n": 9,
		"mcp:\n  discovery_concurrency: -1\n": 4, "mcp:\n  discovery_concurrency: true\n": 4, "mcp:\n  discovery_concurrency: 2.5\n": 4} {
		if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := discoveryConnectConcurrency(ctx, nil); got != want {
			t.Fatalf("%q gave %d want %d", body, got, want)
		}
	}
	if logs.count("is not a non-negative integer") != 3 {
		t.Fatalf("warnings %s", logs.text())
	}
}

func TestStatusStatesAndScopeVisibility(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	configured := map[string]*Rendered{
		"conf": {Config: map[string]any{"command": "x"}}, "off": {Config: map[string]any{"command": "x", "enabled": false}},
		"busy": {Config: map[string]any{"url": "https://e.example/mcp", "transport": "sse"}}, "err": {Config: map[string]any{"url": "https://e.example/mcp"}},
		"lazy": {Config: map[string]any{"command": "x"}}, "foreign": {Config: map[string]any{"command": "x"}},
	}
	mcpMu.Lock()
	serverConnecting[ServerKey{Name: "busy"}] = struct{}{}
	serverConnectErrors[ServerKey{Name: "err"}] = "boom"
	lazyServerConfigs[ServerKey{Name: "lazy"}] = map[string]any{"command": "x"}
	lazyServerToolNames[ServerKey{Name: "lazy"}] = []string{"mcp__lazy__a", "mcp__lazy__b"}
	fk := ServerKey{Scope: "/other", Name: "foreign"}
	serverConnectErrors[fk] = "theirs"
	serverScopeKeys[fk] = "/other"
	mcpMu.Unlock()
	scoped := tools.WithSessionToolScope(ctx, "/mine")
	want := map[string]ServerStatus{
		"conf": {Name: "conf", Transport: "stdio", Status: "configured"}, "off": {Name: "off", Transport: "stdio", Status: "disabled", Disabled: true},
		"err": {Name: "err", Transport: "http", Status: "configured"}, "foreign": {Name: "foreign", Transport: "stdio", Status: "configured"},
	}
	for _, s := range Status(scoped, env, configured, true) {
		if w, ok := want[s.Name]; ok && fmt.Sprint(w) != fmt.Sprint(s) {
			t.Fatalf("scoped %s = %+v", s.Name, s)
		}
	}
	got := map[string]ServerStatus{}
	for _, s := range Status(ctx, env, configured, true) {
		got[s.Name] = s
	}
	if got["err"].Error != "boom" || got["busy"].Status != "connecting" || got["busy"].Transport != "sse" || got["lazy"].Status != "lazy" || got["lazy"].Tools != 2 || got["foreign"].Status != "failed" {
		t.Fatalf("unscoped %+v", got)
	}
	for _, s := range Status(ctx, env, configured, false) {
		if s.Status != "configured" && s.Status != "disabled" {
			t.Fatalf("runtime excluded yet %s is %s", s.Name, s.Status)
		}
	}
	if len(Status(ctx, env, map[string]*Rendered{}, true)) != 0 {
		t.Fatal("empty configuration reports nothing")
	}
	t.Run("test_status_distinguishes_configured_connecting_failed_and_disabled", func(t *testing.T) {
		if c := got["conf"]; c.Status != "configured" || c.Connected || c.Disabled {
			t.Fatalf("configured %+v", c)
		}
		if got["busy"].Status != "connecting" || got["err"].Status != "failed" || got["err"].Error != "boom" || got["off"].Status != "disabled" || !got["off"].Disabled {
			t.Fatalf("states %+v", got)
		}
	})
	t.Run("test_status_ignores_a_runtime_owned_by_another_profile", func(t *testing.T) {
		foreign := newServerTask("shared", env)
		foreign.session = &Session{}
		foreign.toolNames = []string{"secret_tool"}
		key := ServerKey{Scope: "/other", Name: "shared"}
		mcpMu.Lock()
		mcpServers[key] = foreign
		serverScopeKeys[key] = "/other"
		mcpMu.Unlock()
		t.Cleanup(func() {
			mcpMu.Lock()
			delete(mcpServers, key)
			delete(serverScopeKeys, key)
			mcpMu.Unlock()
		})
		shared := map[string]*Rendered{"shared": {Config: map[string]any{"command": "shared-mcp"}}}
		st := Status(scoped, env, shared, true)
		if len(st) != 1 || st[0].Status != "configured" || st[0].Tools != 0 {
			t.Fatalf("scoped %+v", st)
		}
		mcpMu.Lock()
		delete(serverScopeKeys, key)
		mcpMu.Unlock()
		if st = Status(scoped, env, shared, true); len(st) != 1 || st[0].Status != "configured" {
			t.Fatalf("unscoped %+v", st)
		}
	})
}

func TestServerReconnectingPredicate(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	s := newServerTask("p", env)
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "p"}] = s
	mcpMu.Unlock()
	if ServerReconnecting(ctx, "p") {
		t.Fatal("a server that never connected is not reconnecting")
	}
	s.mu.Lock()
	s.everConnected, s.parkReason = true, "connection refused"
	s.mu.Unlock()
	if !ServerReconnecting(ctx, "p") {
		t.Fatal("a transient park is reconnecting")
	}
	s.mu.Lock()
	s.parkReason = "permanent error: 401 Unauthorized"
	s.mu.Unlock()
	if ServerReconnecting(ctx, "p") || ServerReconnecting(ctx, "missing") {
		t.Fatal("a permanent park is blocked")
	}
}

func TestLazyFirstUseConnectLazyConnectFailureRecordsCooldown(t *testing.T) {
	dir, env := managerHome(t)
	ctx := context.Background()
	key := ServerKey{Name: "lf"}
	mcpMu.Lock()
	lazyServerConfigs[key] = map[string]any{"command": filepath.Join(dir, "missing-binary")}
	mcpMu.Unlock()
	if ensureLazyServerConnected(ctx, env, "lf") {
		t.Fatal("a failing lazy connect reports no session")
	}
	mcpMu.Lock()
	cooling, recorded := connectCooldownActive(ctx, "lf"), serverConnectErrors[key] != ""
	mcpMu.Unlock()
	if !cooling || !recorded {
		t.Fatal("a failed lazy connect records its error and cooldown")
	}
	if ensureLazyServerConnected(ctx, env, "lf") {
		t.Fatal("cooldown must block the retry")
	}
}

func TestRegisterParallelSafeIsPerProfile(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	a, b := tools.WithSessionToolScope(ctx, "/a"), tools.WithSessionToolScope(ctx, "/b")
	selectNewServers(a, map[string]*Rendered{"x": {Config: map[string]any{"url": "https://mcp.example/x"}}})
	mcpMu.Lock()
	mcpToolServerNames["mcp__x__t"] = "x"
	mcpMu.Unlock()
	t.Cleanup(func() { mcpMu.Lock(); clear(mcpToolServerNames); mcpMu.Unlock() })
	selectNewServers(b, map[string]*Rendered{"x": {Config: map[string]any{"url": "https://mcp.example/x", "supports_parallel_tool_calls": true}}})
	if !IsToolParallelSafe(b, "mcp__x__t") || IsToolParallelSafe(a, "mcp__x__t") || IsToolParallelSafe(b, "plain") {
		t.Fatal("parallel safety must follow each profile's own opt in")
	}
}

func TestRegisterServedProfileWithoutMultiplexGetsItsOwnConnection(t *testing.T) {
	_, env := managerHome(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	ctxA := home.WithOverride(context.Background(), dirA)
	ctxB := home.WithOverride(context.Background(), dirB)
	srvA := newServerTask("x", env)
	adoptServer(ctxA, "x", srvA)
	keyA := ServerKey{Scope: tools.ScopeKey(dirA), Name: "x"}
	mcpMu.Lock()
	adopted, owner := mcpServers[keyA] == srvA, serverScopeKeys[keyA]
	resolvedB := resolveServerKey(ctxB, "x", "", true)
	mcpMu.Unlock()
	if !adopted || owner != tools.ScopeKey(dirA) || resolvedB == keyA {
		t.Fatalf("adopted %v owner %q resolved %v", adopted, owner, resolvedB)
	}
	if _, ok := selectNewServers(ctxB, map[string]*Rendered{"x": {Config: map[string]any{"url": "https://mcp.example/x"}}})["x"]; !ok {
		t.Fatal("the served profile must connect its own x")
	}
	bare := context.Background()
	if mcpRegistryScope(bare) != "" || serverKey(bare, "x", "", true) != (ServerKey{Name: "x"}) {
		t.Fatal("the launch profile keeps the bare key")
	}
}

// Connects a missing binary once so its first failure arms the cooldown, returning the entry and a clock advance
func failingServerFixture(t *testing.T) (context.Context, map[string]*Rendered, func(time.Duration)) {
	t.Helper()
	dir, env := managerHome(t)
	now := time.Now()
	managerNow = func() time.Time { return now }
	t.Cleanup(func() { managerNow = time.Now })
	ctx := context.Background()
	bad := map[string]*Rendered{"bad": {Config: map[string]any{"command": filepath.Join(dir, "missing-binary")}}}
	if _, err := Connect(ctx, env, bad); err != nil {
		t.Fatal(err)
	}
	mcpMu.Lock()
	failures, deadline := serverConnectFailures[ServerKey{Name: "bad"}], serverConnectRetryAfter[ServerKey{Name: "bad"}]
	mcpMu.Unlock()
	if failures != 1 || deadline.Sub(now) != connectRetryBaseBackoff {
		t.Fatalf("failures %d backoff %v", failures, deadline.Sub(now))
	}
	ShutdownMCPServers(ctx, nil, nil, time.Second)
	ensureMCPLoop()
	mcpMu.Lock()
	recordConnectFailure(ctx, "bad")
	mcpMu.Unlock()
	return ctx, bad, func(d time.Duration) { now = now.Add(d) }
}

// test_mcp_bridge_single_failure.py TestRegisterMcpServersIsolation test_failing_server_skipped_on_second_pass
func TestRegisterMcpServersIsolationFailingServerSkippedOnSecondPass(t *testing.T) {
	ctx, bad, _ := failingServerFixture(t)
	if len(selectNewServers(ctx, bad)) != 0 {
		t.Fatal("a server inside its cooldown is skipped")
	}
	ShutdownMCPServers(ctx, nil, nil, time.Second)
}

// test_mcp_bridge_single_failure.py TestRegisterMcpServersIsolation test_cooldown_expiry_allows_retry
func TestRegisterMcpServersIsolationCooldownExpiryAllowsRetry(t *testing.T) {
	ctx, bad, advance := failingServerFixture(t)
	if len(selectNewServers(ctx, bad)) != 0 {
		t.Fatal("a server inside its cooldown is skipped")
	}
	advance(connectRetryMaxBackoff + time.Second)
	if _, ok := selectNewServers(ctx, bad)["bad"]; !ok {
		t.Fatal("an expired cooldown allows a retry")
	}
	mcpMu.Lock()
	delete(serverConnecting, ServerKey{Name: "bad"})
	mcpMu.Unlock()
	ShutdownMCPServers(ctx, nil, nil, time.Second)
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if len(serverConnectRetryAfter) != 0 || len(serverConnectFailures) != 0 {
		t.Fatal("a full shutdown clears every cooldown")
	}
}
