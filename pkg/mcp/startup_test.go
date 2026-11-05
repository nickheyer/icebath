package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Empties the discovery slots, deferral and filter, restoring them after the test
func resetDiscoveryState(t *testing.T) {
	t.Helper()
	clear := func() {
		discoveryMu.Lock()
		runs := make([]*discoveryRun, 0, len(discoveryRuns))
		for _, r := range discoveryRuns {
			runs = append(runs, r)
		}
		discoveryStarted = map[string]bool{}
		discoveryRuns = map[string]*discoveryRun{}
		discoveryDeferred = nil
		serverFilter = nil
		discoveryMu.Unlock()
		for _, r := range runs {
			select {
			case <-r.done:
			case <-time.After(time.Second):
			}
		}
	}
	clear()
	t.Cleanup(clear)
}

// Writes config.yaml into the home and drops the config cache
func writeStartupConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

// Replaces the discovery entry point with fn for the test
func replaceDiscovery(t *testing.T, fn func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error)) {
	t.Helper()
	prev := discoverMCPTools
	discoverMCPTools = fn
	t.Cleanup(func() { discoverMCPTools = prev })
}

// Pointer to a duration literal
func durp(d time.Duration) *time.Duration { return &d }

func TestStartupResolveTimeoutSingleQueryUsesLargerBound(t *testing.T) {
	dir := isolateHome(t)
	writeStartupConfig(t, dir, "mcp_discovery_timeout: 1.5\nmcp_single_query_discovery_timeout: 25.0\n")
	ctx := context.Background()
	if got := resolveDiscoveryTimeout(ctx, nil, nil, false); got != 1500*time.Millisecond {
		t.Fatalf("interactive bound %v", got)
	}
	if got := resolveDiscoveryTimeout(ctx, nil, nil, true); got != 25*time.Second {
		t.Fatalf("single query bound %v", got)
	}
}

func TestStartupResolveTimeoutSingleQueryFallsBack(t *testing.T) {
	dir := isolateHome(t)
	ctx := context.Background()
	for _, body := range []string{"mcp_single_query_discovery_timeout: 0\n", "mcp_single_query_discovery_timeout: oops\n", "{}\n"} {
		writeStartupConfig(t, dir, body)
		if got := resolveDiscoveryTimeout(ctx, nil, nil, true); got != 15*time.Second {
			t.Fatalf("%q resolved %v", body, got)
		}
	}
	writeStartupConfig(t, dir, "mcp_discovery_timeout: -3\n")
	if got := resolveDiscoveryTimeout(ctx, nil, nil, false); got != 1500*time.Millisecond {
		t.Fatalf("negative bound resolved %v", got)
	}
}

func TestStartupResolveTimeoutExplicitOverridesSingleQuery(t *testing.T) {
	isolateHome(t)
	if got := resolveDiscoveryTimeout(context.Background(), nil, durp(5*time.Second), true); got != 5*time.Second {
		t.Fatalf("explicit bound %v", got)
	}
}

func TestStartupEnsureHelperStartsDiscoveryAndWaits(t *testing.T) {
	dir := isolateHome(t)
	resetDiscoveryState(t)
	writeStartupConfig(t, dir, "mcp_servers:\n  demo:\n    transport: stdio\n    command: demo\n")
	var mu sync.Mutex
	var suppressed []bool
	replaceDiscovery(t, func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
		mu.Lock()
		suppressed = append(suppressed, !isInteractive(ctx))
		mu.Unlock()
		return nil, nil
	})
	var waited []bool
	prev := waitForDiscoveryHook
	waitForDiscoveryHook = func(ctx context.Context, env *tools.Env, timeout *time.Duration, singleQuery bool) {
		waited = append(waited, singleQuery)
		prev(ctx, env, timeout, singleQuery)
	}
	t.Cleanup(func() { waitForDiscoveryHook = prev })
	ctx := ForceInteractiveOAuth(context.Background())
	EnsureDiscoveryBeforeAgentBuild(ctx, nil, nil, nil, true, "")
	if len(waited) != 1 || !waited[0] {
		t.Fatalf("waited %v", waited)
	}
	if !JoinDiscovery(ctx, durp(2*time.Second)) {
		t.Fatal("discovery still running")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(suppressed) != 1 || !suppressed[0] {
		t.Fatalf("discovery ran %d time(s), interactive OAuth suppressed %v", len(suppressed), suppressed)
	}
}

func TestStartupEnsureHelperSwallowsErrors(t *testing.T) {
	dir := isolateHome(t)
	resetDiscoveryState(t)
	writeStartupConfig(t, dir, "mcp_servers: [unterminated\n")
	replaceDiscovery(t, func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
		panic("boom")
	})
	ctx := context.Background()
	EnsureDiscoveryBeforeAgentBuild(ctx, nil, nil, durp(time.Second), false, "")
	if !JoinDiscovery(ctx, durp(2*time.Second)) {
		t.Fatal("discovery still running")
	}
}

func TestStartupWaitStaysBoundedWhenDiscoveryIsSlow(t *testing.T) {
	dir := isolateHome(t)
	resetDiscoveryState(t)
	writeStartupConfig(t, dir, "mcp_single_query_discovery_timeout: 0.1\n")
	ctx := context.Background()
	run := &discoveryRun{done: make(chan struct{})}
	discoveryMu.Lock()
	discoveryRuns[discoveryHomeKey(ctx)] = run
	discoveryMu.Unlock()
	defer close(run.done)
	start := time.Now()
	WaitForDiscovery(ctx, nil, nil, true)
	if elapsed := time.Since(start); elapsed >= 3*time.Second || elapsed < 100*time.Millisecond {
		t.Fatalf("wait took %v", elapsed)
	}
	if !DiscoveryInFlight(ctx) || JoinDiscovery(ctx, durp(10*time.Millisecond)) {
		t.Fatal("stuck discovery reported finished")
	}
}

func TestStartupServerFilter(t *testing.T) {
	resetDiscoveryState(t)
	cases := []struct {
		in   any
		want []string
	}{
		{"github, linear,,", []string{"github", "linear"}},
		{[]string{"a,b", " c "}, []string{"a", "b", "c"}},
		{[]any{"x", 3}, []string{"x", "3"}},
		{"terminal,all", nil},
		{"*", nil},
		{"", nil},
		{nil, nil},
	}
	for _, c := range cases {
		got := SetServerFilter(c.in)
		if len(got) != len(c.want) || (c.want == nil) != (got == nil) {
			t.Fatalf("%v -> %v", c.in, got)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%v -> %v", c.in, got)
			}
		}
	}
}

func TestStartupFilterReachesDiscovery(t *testing.T) {
	dir := isolateHome(t)
	resetDiscoveryState(t)
	writeStartupConfig(t, dir, "mcp_servers:\n  demo:\n    command: demo\n")
	SetServerFilter("demo,terminal")
	got := make(chan []string, 1)
	replaceDiscovery(t, func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
		got <- allowed
		return nil, nil
	})
	ctx := context.Background()
	StartBackgroundDiscovery(ctx, nil, nil, "t")
	select {
	case allowed := <-got:
		if len(allowed) != 2 || allowed[0] != "demo" {
			t.Fatalf("allowed %v", allowed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discovery never ran")
	}
}

func TestStartupRetriesWhenPreviousRunConnectedNothing(t *testing.T) {
	dir := isolateHome(t)
	resetDiscoveryState(t)
	logs := captureLogs(t)
	writeStartupConfig(t, dir, "mcp_servers:\n  demo:\n    command: demo\n")
	var mu sync.Mutex
	calls := 0
	replaceDiscovery(t, func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil, nil
	})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		StartBackgroundDiscovery(ctx, nil, nil, "t")
		if !JoinDiscovery(ctx, durp(2*time.Second)) {
			t.Fatal("discovery still running")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("discovery ran %d time(s)", calls)
	}
	if logs.count("previously exited with no connected servers") != 1 || logs.count("completed with zero connected servers") != 2 {
		t.Fatalf("logs %s", logs.text())
	}
}

func TestStartupSkipsDiscoveryWithoutConfiguredServers(t *testing.T) {
	isolateHome(t)
	resetDiscoveryState(t)
	replaceDiscovery(t, func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
		t.Error("discovery ran without configured servers")
		return nil, nil
	})
	ctx := context.Background()
	StartBackgroundDiscovery(ctx, nil, nil, "t")
	if DiscoveryInFlight(ctx) {
		t.Fatal("discovery in flight")
	}
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	if !discoveryStarted[discoveryHomeKey(ctx)] {
		t.Fatal("slot not claimed")
	}
}

func TestStartupDeferredDiscovery(t *testing.T) {
	dir := isolateHome(t)
	resetDiscoveryState(t)
	writeStartupConfig(t, dir, "mcp_servers:\n  demo:\n    command: demo\n")
	ran := make(chan struct{}, 4)
	replaceDiscovery(t, func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
		ran <- struct{}{}
		return nil, nil
	})
	ctx := context.Background()
	DeferBackgroundDiscovery(ctx, nil, nil, "t", nil)
	DeferBackgroundDiscovery(ctx, nil, nil, "t", durp(time.Millisecond))
	select {
	case <-ran:
		t.Fatal("unclocked deferral fired by itself")
	case <-time.After(50 * time.Millisecond):
	}
	StartDeferredDiscoveryNow()
	StartDeferredDiscoveryNow()
	<-ran
	if !JoinDiscovery(ctx, durp(2*time.Second)) {
		t.Fatal("discovery still running")
	}
	if len(ran) != 0 {
		t.Fatal("deferred start fired twice")
	}
	DeferBackgroundDiscovery(ctx, nil, nil, "t", durp(time.Millisecond))
	discoveryMu.Lock()
	armed := discoveryDeferred != nil
	discoveryMu.Unlock()
	if armed {
		t.Fatal("deferral armed after the home slot was claimed")
	}

	resetDiscoveryState(t)
	DeferBackgroundDiscovery(ctx, nil, nil, "t", durp(5*time.Millisecond))
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("clocked deferral never fired")
	}
	JoinDiscovery(ctx, durp(2*time.Second))
}

func TestStartupDiscoveryRegisteredServers(t *testing.T) {
	if discoveryRegisteredServers(nil) || discoveryRegisteredServers([]ServerStatus{{Status: "failed"}}) {
		t.Fatal("nothing usable reported as registered")
	}
	if !discoveryRegisteredServers([]ServerStatus{{Status: "lazy"}}) || !discoveryRegisteredServers([]ServerStatus{{Connected: true}}) {
		t.Fatal("usable server not reported")
	}
}

func TestStartupAuthParkedOAuthServerIgnoresDiscoveryUntilCredentialsChange(t *testing.T) {
	dir := isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	maxInitialConnectRetries = 0
	parkedRetryInterval = time.Hour
	ctx := context.Background()
	cfg := map[string]any{"url": "https://mcp.example.test/mcp", "auth": "oauth"}
	tokenFile := NewTokenStorage(ctx, "cloud", dir).TokensPath()
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(`{"access_token": "a", "token_type": "Bearer", "refresh_token": "dead"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, calls := runnerTask(t, "cloud", cfg, func(s *serverTask, ctx context.Context, call int) (string, error) {
		return "", &OAuthNonInteractiveError{Msg: "refresh token rejected; browser authorization required"}
	})
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "cloud"}] = s
	mcpMu.Unlock()
	_ = s.start(ctx, s.rendered)
	waitUntil(t, "the server never parked on the auth failure", func() bool { return calls.Load() == 1 && s.parked() })
	entry := map[string]*Rendered{"cloud": {Config: cfg}}
	for i := 0; i < 5; i++ {
		selectNewServers(ctx, entry)
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("discovery passes replayed the rejected credentials %d time(s)", calls.Load())
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(tokenFile, []byte(`{"access_token": "b", "token_type": "Bearer", "refresh_token": "new"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	selectNewServers(ctx, entry)
	waitUntil(t, "rewritten credentials never revived the parked server", func() bool { return calls.Load() == 2 })
	s.shutdown(ctx)
}
