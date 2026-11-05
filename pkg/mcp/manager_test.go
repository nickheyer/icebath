package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// Mock stdio server entry, extra keys merged in
func mockEntry(t *testing.T, extra map[string]any) *Rendered {
	cfg := map[string]any{"command": mockBinary(t), "args": []any{}}
	for k, v := range extra {
		cfg[k] = v
	}
	return &Rendered{Config: cfg}
}

// Config yaml body naming one mock server per name
func mockServersYAML(t *testing.T, names ...string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "  %s:\n    command: %q\n", n, mockBinary(t))
	}
	return b.String()
}

func TestManagerConnectCooldownDoublesToCap(t *testing.T) {
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
	before := shimHydrateCalls
	ownerSecretMapping(dir)
	ownerSecretMapping(dir)
	if shimHydrateCalls-before != 1 {
		t.Fatalf("hydrated %d times inside the interval", shimHydrateCalls-before)
	}
	now = now.Add(hydrateRetryInterval)
	ownerSecretMapping(dir)
	if shimHydrateCalls-before != 2 {
		t.Fatal("hydration must retry after the interval")
	}
}

func TestDiscoveryCrossProcessLock(t *testing.T) {
	dir := isolateHome(t)
	logs := captureLogs(t)
	ctx := home.WithOverride(context.Background(), dir)
	cookie := acquireDiscoveryLockWithRetry(ctx)
	if cookie == nil {
		t.Fatal("free lock must be acquired")
	}
	sleeps := 0
	discoveryLockSleep = func() { sleeps++ }
	t.Cleanup(func() { discoveryLockSleep = func() { time.Sleep(discoveryLockRetryDelay) } })
	if got := acquireDiscoveryLockWithRetry(ctx); got != nil {
		t.Fatal("held lock must fall back to unguarded")
	}
	if sleeps != discoveryLockMaxRetries || logs.count("still held after 620 retries") != 1 {
		t.Fatalf("slept %d times, logs %s", sleeps, logs.text())
	}
	cookie.Release()
	again := acquireDiscoveryLockWithRetry(ctx)
	if again == nil {
		t.Fatal("released lock must be acquirable")
	}
	again.Release()
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

func TestRegisterConnectsNewServersIdempotently(t *testing.T) {
	_, env := managerHome(t)
	logs := captureLogs(t)
	ctx := context.Background()
	servers := map[string]*Rendered{"a": mockEntry(t, map[string]any{"supports_parallel_tool_calls": true})}
	names, err := Connect(ctx, env, servers)
	if err != nil || !slices.Equal(names, []string{"mcp__a__tool0"}) {
		t.Fatalf("names %v err %v", names, err)
	}
	if logs.count("MCP: registered 1 tool(s) from 1 server(s)") != 1 || !IsToolParallelSafe(ctx, "mcp__a__tool0") {
		t.Fatalf("summary or parallel flag missing: %s", logs.text())
	}
	mcpMu.Lock()
	first := mcpServers[ServerKey{Name: "a"}]
	mcpMu.Unlock()
	if _, err := Connect(ctx, env, servers); err != nil {
		t.Fatal(err)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if mcpServers[ServerKey{Name: "a"}] != first || len(serverConnecting) != 0 {
		t.Fatal("a connected server must not reconnect")
	}
}

func TestRegisterSkipsServersAlreadyConnecting(t *testing.T) {
	_, env := managerHome(t)
	mcpMu.Lock()
	serverConnecting[ServerKey{Name: "busy"}] = struct{}{}
	mcpMu.Unlock()
	if _, err := Connect(context.Background(), env, map[string]*Rendered{"busy": mockEntry(t, nil)}); err != nil {
		t.Fatal(err)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if _, ok := mcpServers[ServerKey{Name: "busy"}]; ok {
		t.Fatal("a connecting server must not be connected twice")
	}
}

func TestRegisterWakesStaleCachedServer(t *testing.T) {
	_, env := managerHome(t)
	stale := newServerTask("stale", env)
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "stale"}] = stale
	mcpMu.Unlock()
	if _, err := Connect(context.Background(), env, map[string]*Rendered{"stale": mockEntry(t, nil)}); err != nil {
		t.Fatal(err)
	}
	if !stale.reconnect.IsSet() {
		t.Fatal("a known server without a session must be signalled to reconnect")
	}
}

func TestRegisterSummaryNamesFailuresAndCooldown(t *testing.T) {
	dir, env := managerHome(t)
	logs := captureLogs(t)
	writeServersConfig(t, mockServersYAML(t, "good")+"  bad:\n    command: "+filepath.Join(dir, "missing-binary")+"\n  cool:\n    command: nothing\n")
	ctx := context.Background()
	mcpMu.Lock()
	recordConnectFailure(ctx, "cool")
	mcpMu.Unlock()
	if _, err := ConnectAll(ctx, env, nil); err != nil {
		t.Fatal(err)
	}
	text := logs.text()
	if !strings.Contains(text, "MCP: registered 1 tool(s) from 1 server(s) (1 failed: bad (") {
		t.Fatalf("register summary %s", text)
	}
	if !strings.Contains(text, "  MCP: 1 tool(s) from 1 server(s) (2 failed: bad (") || !strings.Contains(text, "cool (not attempted (in retry cooldown))") {
		t.Fatalf("discover summary %s", text)
	}
	st := Status(ctx, env, nil, true)
	got := map[string]string{}
	for _, s := range st {
		got[s.Name] = s.Status
	}
	if got["good"] != "connected" || got["bad"] != "failed" || got["cool"] != "configured" {
		t.Fatalf("status %v", got)
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

func TestLazyRegistersFromCacheAndConnectsOnFirstUse(t *testing.T) {
	_, env := managerHome(t)
	logs := captureLogs(t)
	ctx := context.Background()
	r := mockEntry(t, map[string]any{"lazy": true})
	cached := []any{map[string]any{"name": "tool0"}, map[string]any{"name": "ghost"}}
	if err := WriteCacheEntry(ctx, "lz", ConfigFingerprint(r.Config), cached, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	names, err := Connect(ctx, env, map[string]*Rendered{"lz": r})
	if err != nil || len(names) != 2 || logs.count("registered 2 lazy tool(s) from schema cache") != 1 {
		t.Fatalf("names %v err %v logs %s", names, err, logs.text())
	}
	mcpMu.Lock()
	_, live := mcpServers[ServerKey{Name: "lz"}]
	mcpMu.Unlock()
	if live || Status(ctx, env, map[string]*Rendered{"lz": r}, true)[0].Status != "lazy" {
		t.Fatal("a lazy server must not spawn at registration")
	}
	if _, err := Connect(ctx, env, map[string]*Rendered{"lz": r}); err != nil {
		t.Fatal(err)
	}
	srv := getConnectedServerForCall(ctx, env, "lz")
	if srv == nil || srv.currentSession() == nil {
		t.Fatal("first use must connect the lazy server")
	}
	mcpMu.Lock()
	_, stillLazy := lazyServerConfigs[ServerKey{Name: "lz"}]
	_, ghostKept := mcpToolServerNames["mcp__lz__ghost"]
	phantom := !ghostKept
	mcpMu.Unlock()
	if stillLazy || !phantom || logs.count("deregistered 1 phantom cached tool(s)") != 1 {
		t.Fatalf("lazy state %v phantom %v logs %s", stillLazy, phantom, logs.text())
	}
}

func TestLazyConnectFailureRecordsCooldown(t *testing.T) {
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

func TestReconcileTearsDownDroppedAndForgetsLazy(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	writeServersConfig(t, mockServersYAML(t, "keep", "drop"))
	if _, err := ConnectAll(ctx, env, nil); err != nil {
		t.Fatal(err)
	}
	mcpMu.Lock()
	lazyServerConfigs[ServerKey{Name: "gone"}] = map[string]any{"command": "x"}
	lazyServerToolNames[ServerKey{Name: "gone"}] = []string{"mcp__gone__t"}
	serverConnecting[ServerKey{Name: "mid"}] = struct{}{}
	serverScopeKeys[ServerKey{Name: "mid"}] = ""
	mcpMu.Unlock()
	writeServersConfig(t, mockServersYAML(t, "keep", "fresh"))
	res, err := Reconcile(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(res) != "{[drop gone] [fresh] [mid]}" {
		t.Fatalf("reconcile %+v", res)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	_, dropLive := mcpServers[ServerKey{Name: "drop"}]
	_, freshLive := mcpServers[ServerKey{Name: "fresh"}]
	if dropLive || !freshLive {
		t.Fatal("reconcile must tear down the dropped server and connect the new one")
	}
}

func TestRestartOnlyWhenResolvedInputsChange(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	writeServersConfig(t, mockServersYAML(t, "a"))
	if _, err := ConnectAll(ctx, env, nil); err != nil {
		t.Fatal(err)
	}
	mcpMu.Lock()
	first := mcpServers[ServerKey{Name: "a"}]
	mcpMu.Unlock()
	if got, err := Restart(ctx, env, []string{"a"}); err != nil || len(got) != 0 {
		t.Fatalf("unchanged inputs restarted %v %v", got, err)
	}
	writeServersConfig(t, mockServersYAML(t, "a")+"    env:\n      MOCK_MCP_PAGES: \"1\"\n")
	if got, err := Restart(ctx, env, []string{"a"}); err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("changed inputs restarted %v %v", got, err)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if second := mcpServers[ServerKey{Name: "a"}]; second == nil || second == first {
		t.Fatal("a changed server must reconnect")
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

func TestRegisterFailingServerBacksOffThenRetries(t *testing.T) {
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
	if len(selectNewServers(ctx, bad)) != 0 {
		t.Fatal("a server inside its cooldown is skipped")
	}
	now = now.Add(connectRetryMaxBackoff + time.Second)
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

func TestDiscoveryConnectTimeoutReportsTimeoutError(t *testing.T) {
	_, env := managerHome(t)
	r := mockEntry(t, map[string]any{"connect_timeout": 0.000001})
	_, err := discoverAndRegisterServer(context.Background(), env, "slow", r)
	if err == nil || formatConnectError(err) != "TimeoutError" {
		t.Fatalf("connect timeout reported as %v", err)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if _, ok := mcpServers[ServerKey{Name: "slow"}]; ok {
		t.Fatal("a timed out connect is reaped, never adopted")
	}
}
