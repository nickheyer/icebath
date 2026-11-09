package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Child process whose argv carries the given trailing arguments, killed when the test ends
func spawnArgvChild(t *testing.T, args ...string) int {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", "sleep 30"}, args...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// test_mcp_tool.py TestFilterMCPChildren test_filters_gateway_children_by_argv_marker
func TestFilterMCPChildrenFiltersGatewayChildrenByArgvMarker(t *testing.T) {
	gateway := spawnArgvChild(t, "-m", "tui_gateway.slash_worker", "--session-key", "abc")
	jdtls := spawnArgvChild(t, "-jar", "/opt/jdtls/plugins/org.eclipse.equinox.launcher_1.7.0.jar")
	node := spawnArgvChild(t, "server.js")
	kept := filterMCPChildren(map[int]bool{gateway: true, jdtls: true, node: true})
	if len(kept) != 1 || !kept[node] {
		t.Fatalf("kept %v, node pid %d", kept, node)
	}
}

// Registers an already connected stand in task under name so a register pass refreshes rather than spawns it
func liveStandIn(t *testing.T, env *tools.Env, name string) *serverTask {
	t.Helper()
	live := newServerTask(name, env)
	live.session = &Session{}
	mcpMu.Lock()
	mcpServers[ServerKey{Name: name}] = live
	mcpMu.Unlock()
	return live
}

func parallelSafe(name string) bool {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	_, ok := parallelSafeServers[ServerKey{Name: name}]
	return ok
}

// test_mcp_tool.py TestMCPParallelSafetyProvenance test_parallel_safe_servers_keep_exact_raw_names
func TestMCPParallelSafetyProvenanceParallelSafeServersKeepExactRawNames(t *testing.T) {
	_, env := managerHome(t)
	liveStandIn(t, env, "foo-bar")
	liveStandIn(t, env, "foo_bar")
	t.Cleanup(func() {
		mcpMu.Lock()
		clear(parallelSafeServers)
		mcpMu.Unlock()
	})
	if _, err := Connect(context.Background(), env, map[string]*Rendered{
		"foo-bar": {Config: map[string]any{"command": "x", "supports_parallel_tool_calls": true}},
		"foo_bar": {Config: map[string]any{"command": "x", "supports_parallel_tool_calls": false}},
	}); err != nil {
		t.Fatal(err)
	}
	if !parallelSafe("foo-bar") || parallelSafe("foo_bar") {
		t.Fatalf("parallel safe foo-bar %v foo_bar %v", parallelSafe("foo-bar"), parallelSafe("foo_bar"))
	}
}

// test_mcp_tool.py TestMCPParallelSafetyProvenance test_tool_provenance_keeps_exact_raw_server_names
func TestMCPParallelSafetyProvenanceToolProvenanceKeepsExactRawServerNames(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	reset := func() {
		mcpMu.Lock()
		clear(mcpToolServerNames)
		clear(parallelSafeServers)
		mcpMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	mcpMu.Lock()
	parallelSafeServers[ServerKey{Name: "foo-bar"}] = struct{}{}
	mcpMu.Unlock()
	trackMCPToolServer("mcp__foo_bar__first", "foo-bar")
	trackMCPToolServer("mcp__foo_bar__second", "foo_bar")
	if !IsToolParallelSafe(ctx, "mcp__foo_bar__first") || IsToolParallelSafe(ctx, "mcp__foo_bar__second") {
		t.Fatal("parallel safety must follow the exact raw server name")
	}
	if names := RegisteredServerNames(); len(names) != 2 || !names["foo-bar"] || !names["foo_bar"] {
		t.Fatalf("registered server names %v", names)
	}
}

// test_mcp_tool.py TestMcpParallelToolCalls test_register_mcp_servers_tracks_parallel_flag
func TestMcpParallelToolCallsRegisterMcpServersTracksParallelFlag(t *testing.T) {
	_, env := managerHome(t)
	servers := map[string]*Rendered{
		"parallel_srv": mockEntry(t, map[string]any{"supports_parallel_tool_calls": true}),
		"serial_srv":   mockEntry(t, map[string]any{"supports_parallel_tool_calls": false}),
		"default_srv":  mockEntry(t, nil),
	}
	if _, err := Connect(context.Background(), env, servers); err != nil {
		t.Fatal(err)
	}
	if !parallelSafe("parallel_srv") || parallelSafe("serial_srv") || parallelSafe("default_srv") {
		t.Fatal("only the server opting in is parallel safe")
	}
}

// test_mcp_tool.py TestDiscoverAndRegister test_tools_registered_in_registry
func TestDiscoverAndRegisterToolsRegisteredInRegistry(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	names, err := Connect(ctx, env, map[string]*Rendered{"fs": mockEntry(t, map[string]any{"env": map[string]any{"MOCK_MCP_TOOLS": "read_file,write_file"}})})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"mcp__fs__read_file", "mcp__fs__write_file"} {
		if !slices.Contains(names, n) || tools.Default.Lookup(ctx, "", n) == nil {
			t.Fatalf("%s missing from %v", n, names)
		}
	}
}

// test_mcp_tool.py TestMCPServerTask test_start_connects_and_discovers_tools
func TestMCPServerTaskStartConnectsAndDiscoversTools(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	s := newServerTask("test_srv", env)
	if err := s.start(ctx, mockEntry(t, map[string]any{"env": map[string]any{"MOCK_MCP_TOOLS": "echo"}, "cwd": t.TempDir()})); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	names := toolNames(s.tools)
	s.mu.Unlock()
	if s.currentSession() == nil || strings.Join(names, ",") != "echo" {
		t.Fatalf("session %v tools %v", s.currentSession(), names)
	}
	s.shutdown(ctx)
	if s.currentSession() != nil {
		t.Fatal("shutdown left the session")
	}
}

// Real path of a directory for cwd comparisons
func realDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// test_mcp_tool.py TestMCPServerTask test_start_preserves_native_default_cwd
func TestMCPServerTaskStartPreservesNativeDefaultCwd(t *testing.T) {
	isolateHome(t)
	t.Setenv("TERMINAL_CWD", "")
	_, _, cwd, err := stdioLaunch(context.Background(), &tools.Env{Values: &sync.Map{}}, mockEntry(t, nil))
	if err != nil || cwd != "" {
		t.Fatalf("cwd %q err %v", cwd, err)
	}
}

// test_mcp_tool.py TestMCPServerTask test_start_defaults_stdio_cwd_to_session_cwd
func TestMCPServerTaskStartDefaultsStdioCwdToSessionCwd(t *testing.T) {
	isolateHome(t)
	t.Setenv("TERMINAL_CWD", "")
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Values: &sync.Map{}}
	tools.SetLogicalSessionCwd(env, workspace)
	defer tools.ClearLogicalSessionCwd(env)
	_, _, cwd, err := stdioLaunch(context.Background(), env, mockEntry(t, nil))
	if err != nil || realDir(t, cwd) != realDir(t, workspace) {
		t.Fatalf("cwd %q err %v", cwd, err)
	}
}

// test_mcp_tool.py TestMCPServerTask test_start_configured_cwd_overrides_session_cwd
func TestMCPServerTaskStartConfiguredCwdOverridesSessionCwd(t *testing.T) {
	isolateHome(t)
	t.Setenv("TERMINAL_CWD", "")
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Values: &sync.Map{}}
	tools.SetLogicalSessionCwd(env, workspace)
	defer tools.ClearLogicalSessionCwd(env)
	_, _, cwd, err := stdioLaunch(context.Background(), env, mockEntry(t, map[string]any{"cwd": "/plugin"}))
	if err != nil || cwd != "/plugin" {
		t.Fatalf("cwd %q err %v", cwd, err)
	}
}

// test_mcp_tool.py TestMCPServerTask test_stdio_recycle_deadline_pauses_while_rpc_active
func TestMCPServerTaskStdioRecycleDeadlinePausesWhileRpcActive(t *testing.T) {
	isolateHome(t)
	s := newServerTask("srv", nil)
	idle := 0.01
	s.config = map[string]any{"command": "npx"}
	s.idleTimeout = &idle
	s.lastToolCallAt = serverNow().Add(-time.Second)
	if err := s.rpc.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.rpc.Unlock()
	if reason := s.stdioRecycleReason(serverNow()); reason != "" {
		t.Fatalf("recycle reason %q while an rpc holds the lock", reason)
	}
	if _, ok := s.nextStdioRecycleDeadline(); ok {
		t.Fatal("a recycle deadline surfaced while an rpc holds the lock")
	}
}

// Mock stdio server YAML entry exposing the named tools, extra lines appended under it
func mockServerYAML(t *testing.T, name, toolList, extra string) string {
	t.Helper()
	return fmt.Sprintf("  %s:\n    command: %s\n    env:\n      MOCK_MCP_TOOLS: %s\n%s", name, mockBinary(t), toolList, extra)
}

// Writes the mcp_servers block and drops the cached config so the next load reads it
func writeMCPServers(t *testing.T, body string) {
	t.Helper()
	writeServersConfig(t, body)
	config.ResetCache()
}

// test_mcp_tool.py TestToolsetInjection test_mcp_tools_resolve_through_server_aliases
func TestToolsetInjectionMcpToolsResolveThroughServerAliases(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	writeMCPServers(t, mockServerYAML(t, "fs", "list_files", ""))
	names, err := ConnectAll(ctx, env, nil)
	if err != nil || !slices.Contains(names, "mcp__fs__list_files") {
		t.Fatalf("names %v err %v", names, err)
	}
	for _, alias := range []string{"fs", "mcp-fs"} {
		if !tools.Default.ValidateToolset(ctx, "", alias) || !slices.Contains(tools.Default.ResolveToolset(ctx, "", alias), "mcp__fs__list_files") {
			t.Fatalf("alias %s does not resolve to the discovered tool", alias)
		}
	}
}

// Endpoint answering HTML until fixed, then serving the Streamable HTTP mock
func fixableEndpoint(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var fixed atomic.Bool
	var posts atomic.Int64
	inner := jsonMCPHandler(&posts)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fixed.Load() {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html></html>"))
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &fixed
}

// test_mcp_tool.py TestToolsetInjection test_partial_failure_retry_on_second_call
func TestToolsetInjectionPartialFailureRetryOnSecondCall(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	broken, fixed := fixableEndpoint(t)
	writeMCPServers(t, "  broken:\n    url: "+broken.URL+"/mcp\n"+mockServerYAML(t, "good", "ping", ""))
	first, err := ConnectAll(ctx, env, nil)
	if err != nil || !slices.Contains(first, "mcp__good__ping") || slices.Contains(first, "mcp__broken__echo") || serverSpawned("broken") {
		t.Fatalf("first pass %v err %v spawned %v", first, err, serverSpawned("broken"))
	}
	mcpMu.Lock()
	good := mcpServers[ServerKey{Name: "good"}]
	delete(serverConnectRetryAfter, ServerKey{Name: "broken"})
	mcpMu.Unlock()
	fixed.Store(true)
	second, err := ConnectAll(ctx, env, nil)
	if err != nil || !slices.Contains(second, "mcp__good__ping") || !slices.Contains(second, "mcp__broken__echo") {
		t.Fatalf("second pass %v err %v", second, err)
	}
	mcpMu.Lock()
	same := mcpServers[ServerKey{Name: "good"}] == good
	mcpMu.Unlock()
	if good == nil || !same {
		t.Fatal("the good server was reconnected on the retry pass")
	}
}

// test_mcp_tool.py TestShutdown test_shutdown_deregisters_registered_tools
func TestShutdownShutdownDeregistersRegisteredTools(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	def := testToolDef("mcp__test__ping", "mcp-test")
	def.Schema = map[string]any{"name": "mcp__test__ping", "description": "Ping", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	tools.Default.Remove(def.Name)
	tools.Default.Register(def)
	t.Cleanup(func() { tools.Default.Remove(def.Name) })
	tools.Default.RegisterToolsetAlias("test", "mcp-test")
	s := newServerTask("test", env)
	s.toolNames = []string{"mcp__test__ping"}
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "test"}] = s
	mcpMu.Unlock()
	ensureMCPLoop()
	if !tools.Default.ValidateToolset(ctx, "", "test") || !slices.Contains(tools.Default.ResolveToolset(ctx, "", "test"), "mcp__test__ping") {
		t.Fatal("the raw alias must resolve before shutdown")
	}
	ShutdownMCPServers(ctx, nil, nil, 5*time.Second)
	if tools.Default.Lookup(ctx, "", "mcp__test__ping") != nil || tools.Default.ValidateToolset(ctx, "", "test") {
		t.Fatal("shutdown left the MCP tool or its raw alias registered")
	}
}

// test_mcp_tool.py TestShutdown test_shutdown_drains_parked_server_after_bounded_wait_expires
func TestShutdownShutdownDrainsParkedServerAfterBoundedWaitExpires(t *testing.T) {
	managerHome(t)
	shutdownWait = time.Hour
	logs := captureLogs(t)
	s, _ := runnerTask(t, "parked", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		fakeConnected(s, false)
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "parked"}] = s
	mcpMu.Unlock()
	ShutdownMCPServers(context.Background(), nil, nil, 100*time.Millisecond)
	if runningLoop() != nil {
		t.Fatal("the loop owner left the loop running")
	}
	waitUntil(t, "parked MCPServerTask was not drained before its loop closed", func() bool { return taskDone(s) })
	if logs.count("Error during MCP shutdown: timed out") != 1 {
		t.Fatalf("bounded wait never expired: %s", logs.text())
	}
}

// Serving task over a fake transport runner with the loop and fast timings in place
func fakeTransportTask(t *testing.T, name string, cfg map[string]any, runner func(s *serverTask, ctx context.Context, call int) (string, error)) (*serverTask, *atomic.Int64) {
	t.Helper()
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	return runnerTask(t, name, cfg, runner)
}

// Runner body that stays connected until shutdown
func serveUntilShutdown(s *serverTask) (string, error) {
	fakeConnected(s, false)
	<-s.shutdownEv.Wait()
	return "shutdown", nil
}

// test_mcp_tool.py TestReconnection test_reconnect_on_disconnect
func TestReconnectionReconnectOnDisconnect(t *testing.T) {
	s, calls := fakeTransportTask(t, "test_srv", map[string]any{"command": "test"}, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call == 1 {
			fakeConnected(s, false)
			return "", errors.New("connection dropped")
		}
		s.shutdownEv.Set()
		return serveUntilShutdown(s)
	})
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the run loop never ended", func() bool { return taskDone(s) })
	if calls.Load() < 2 {
		t.Fatalf("transport entered %d times, a drop must reconnect", calls.Load())
	}
}

// test_mcp_tool.py TestReconnection test_reconnect_failures_after_initial_success_use_reconnect_budget
func TestReconnectionReconnectFailuresAfterInitialSuccessUseReconnectBudget(t *testing.T) {
	s, _ := fakeTransportTask(t, "test_srv", map[string]any{"command": "test"}, func(s *serverTask, ctx context.Context, call int) (string, error) {
		switch {
		case call == 1:
			fakeConnected(s, false)
			return "reconnect", nil
		case call <= 5:
			return "", errors.New("reconnect attempt failed")
		}
		s.shutdownEv.Set()
		return serveUntilShutdown(s)
	})
	maxInitialConnectRetries, maxReconnectRetries = 3, 5
	parkedRetryInterval = 10 * time.Millisecond
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the run loop never ended", func() bool { return taskDone(s) })
	if s.parked() {
		t.Fatal("server parked after only 4 reconnect failures following a successful initial connection -- it was misclassified as never having connected and re-entered the initial-connect ladder instead of the (larger) reconnect budget")
	}
}

// test_mcp_tool.py TestReconnection test_preflight_probe_runs_on_initial_http_connect
func TestReconnectionPreflightProbeRunsOnInitialHttpConnect(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	s, _ := fakeTransportTask(t, "http_srv", map[string]any{"url": srv.URL + "/mcp"}, func(s *serverTask, ctx context.Context, call int) (string, error) {
		s.shutdownEv.Set()
		return serveUntilShutdown(s)
	})
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the run loop never ended", func() bool { return taskDone(s) })
	if probes.Load() != 1 {
		t.Fatalf("the preflight probe hit the endpoint %d times", probes.Load())
	}
}

// test_mcp_tool.py TestConfigurableTimeouts test_custom_timeout
func TestConfigurableTimeoutsCustomTimeout(t *testing.T) {
	s, _ := fakeTransportTask(t, "test_srv", map[string]any{"command": "test", "timeout": 180}, func(s *serverTask, ctx context.Context, call int) (string, error) {
		return serveUntilShutdown(s)
	})
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	timeout := s.toolTimeout
	s.mu.Unlock()
	s.shutdown(context.Background())
	if timeout != 180 {
		t.Fatalf("tool timeout %v", timeout)
	}
}

// test_mcp_tool.py TestMCPSelectiveToolLoading test_include_takes_precedence_over_exclude
func TestMCPSelectiveToolLoadingIncludeTakesPrecedenceOverExclude(t *testing.T) {
	env, ctx := regSetup(t)
	s := regServer(env, "ink", capsOf(false, false), regTool("create_service", nil), regTool("delete_service", nil), regTool("list_services", nil))
	got := mustRegister(t, ctx, s, map[string]any{"url": "https://mcp.example.com",
		"tools": map[string]any{"include": []any{"create_service"}, "exclude": []any{"create_service", "delete_service"}}})
	if !slices.Equal(got, []string{"mcp__ink__create_service"}) {
		t.Fatalf("registered %v", got)
	}
}

// test_mcp_tool.py TestMCPSelectiveToolLoading test_empty_include_registers_nothing
func TestMCPSelectiveToolLoadingEmptyIncludeRegistersNothing(t *testing.T) {
	env, ctx := regSetup(t)
	s := regServer(env, "ink", capsOf(false, false), regTool("create_service", nil), regTool("delete_service", nil), regTool("list_services", nil))
	if got := mustRegister(t, ctx, s, map[string]any{"url": "https://mcp.example.com", "tools": map[string]any{"include": []any{}}}); len(got) != 0 {
		t.Fatalf("an empty include whitelist registered %v", got)
	}
}

// test_mcp_tool.py TestMCPSelectiveToolLoading test_enabled_false_skips_connection_attempt
func TestMCPSelectiveToolLoadingEnabledFalseSkipsConnectionAttempt(t *testing.T) {
	_, env := managerHome(t)
	writeMCPServers(t, "  ink:\n    url: https://mcp.example.com\n    enabled: false\n")
	names, err := ConnectAll(context.Background(), env, nil)
	mcpMu.Lock()
	_, connecting := serverConnecting[ServerKey{Name: "ink"}]
	mcpMu.Unlock()
	if err != nil || len(names) != 0 || serverSpawned("ink") || connecting {
		t.Fatalf("names %v err %v spawned %v connecting %v", names, err, serverSpawned("ink"), connecting)
	}
}

// One discovery pass over count fake servers under a connect cap, giving the peak in flight connects and connected names
func discoveryPassPeak(t *testing.T, env *tools.Env, count, limit int) (int32, []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(fmt.Sprintf("mcp:\n  discovery_concurrency: %d\n", limit)), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	ensureMCPLoop()
	var inflight, peak atomic.Int32
	var mu sync.Mutex
	var connected []string
	prev := newServerTaskFn
	newServerTaskFn = func(name string, env *tools.Env) *serverTask {
		s := newServerTask(name, env)
		s.transportRunner = func(ctx context.Context, r *Rendered) (string, error) {
			n := inflight.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(50 * time.Millisecond)
			inflight.Add(-1)
			mu.Lock()
			connected = append(connected, name)
			mu.Unlock()
			return serveUntilShutdown(s)
		}
		return s
	}
	defer func() { newServerTaskFn = prev }()
	servers := map[string]*Rendered{}
	for i := range count {
		servers[fmt.Sprintf("srv%d", i)] = &Rendered{Config: map[string]any{"command": "npx", "args": []any{fmt.Sprintf("s%d", i)}}}
	}
	if err := discoveryPass(context.Background(), env, servers); err != nil {
		t.Fatal(err)
	}
	ShutdownMCPServers(context.Background(), nil, nil, 5*time.Second)
	mu.Lock()
	defer mu.Unlock()
	return peak.Load(), slices.Sorted(slices.Values(connected))
}

// test_mcp_tool.py TestDiscoveryConnectConcurrency test_configured_cap_bounds_in_flight_connects_and_zero_means_unlimited
func TestDiscoveryConnectConcurrencyConfiguredCapBoundsInFlightConnectsAndZeroMeansUnlimited(t *testing.T) {
	_, env := managerHome(t)
	peak, connected := discoveryPassPeak(t, env, 8, 3)
	if peak <= 1 || peak > 3 || len(connected) != 8 {
		t.Fatalf("in-flight connects peaked at %d, cap is 3, connected %v", peak, connected)
	}
	peak, connected = discoveryPassPeak(t, env, 8, 0)
	if peak != 8 || len(connected) != 8 {
		t.Fatalf("0 must mean unlimited, peaked at %d, connected %v", peak, connected)
	}
}

// test_mcp_tool.py TestDiscoveryConnectConcurrency test_pass_timeout_capped_by_waiter_budget
func TestDiscoveryConnectConcurrencyPassTimeoutCappedByWaiterBudget(t *testing.T) {
	budget := time.Duration(discoveryLockMaxRetries) * discoveryLockRetryDelay
	if budget <= discoveryPassMax {
		t.Fatalf("waiter budget %v must outlast the pass ceiling %v or a slow pass re-opens unguarded discovery", budget, discoveryPassMax)
	}
	if got := discoveryPassTimeout(40, 3); got != discoveryPassMax {
		t.Fatalf("pass timeout must be capped at %v, got %v (uncapped: %v vs waiter budget %v)", discoveryPassMax, got, 14*discoveryWaveTimeout, budget)
	}
}

// test_mcp_tool.py TestSanitizeMcpNameComponent test_slash_in_server_alias_resolution
func TestSanitizeMcpNameComponentSlashInServerAliasResolution(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	def := testToolDef("mcp__ai_exa_exa__search", "mcp-ai.exa/exa")
	def.Schema = map[string]any{"name": "mcp__ai_exa_exa__search", "description": "Search", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	tools.Default.Remove(def.Name)
	tools.Default.Register(def)
	t.Cleanup(func() { tools.Default.Remove(def.Name) })
	tools.Default.RegisterToolsetAlias("ai.exa/exa", "mcp-ai.exa/exa")
	if !tools.Default.ValidateToolset(ctx, "", "ai.exa/exa") || !slices.Contains(tools.Default.ResolveToolset(ctx, "", "ai.exa/exa"), "mcp__ai_exa_exa__search") {
		t.Fatal("a server name with slashes must resolve through its live alias")
	}
}

// Headers the owned client leaves on the redirect follow up from the origin to location
func redirectHop(t *testing.T, strict bool, configured map[string]bool, headers map[string]string, location string) http.Header {
	t.Helper()
	oc, err := newOwnedClient(HTTPOptions{URL: "https://origin.example.test/mcp", StrictRedirectHeaders: strict, ConfiguredHeaderNames: configured, SSLVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := http.NewRequest(http.MethodGet, "https://origin.example.test/mcp", nil)
	next, _ := http.NewRequest(http.MethodGet, location, nil)
	for k, v := range headers {
		first.Header.Set(k, v)
		next.Header.Set(k, v)
	}
	if err := oc.checkRedirect(next, []*http.Request{first}); err != nil {
		t.Fatal(err)
	}
	return next.Header
}

// test_mcp_tool.py TestRedirectHeaderStripper test_default_strips_only_authorization
func TestRedirectHeaderStripperDefaultStripsOnlyAuthorization(t *testing.T) {
	h := redirectHop(t, false, nil, map[string]string{"Authorization": "Bearer x", "X-Tenant": "t"}, "https://other.example.test/mcp")
	if h.Get("Authorization") != "" || h.Get("X-Tenant") != "t" {
		t.Fatalf("headers %v", h)
	}
}

// test_mcp_tool.py TestRedirectHeaderStripper test_strict_strips_configured_headers_cross_origin
func TestRedirectHeaderStripperStrictStripsConfiguredHeadersCrossOrigin(t *testing.T) {
	h := redirectHop(t, true, map[string]bool{"x-tenant": true}, map[string]string{"Authorization": "Bearer x", "X-Tenant": "t", "Accept": "a"}, "https://other.example.test/mcp")
	if h.Get("Authorization") != "" || h.Get("X-Tenant") != "" || h.Get("Accept") != "a" {
		t.Fatalf("headers %v", h)
	}
}

// test_mcp_tool.py TestRedirectHeaderStripper test_same_origin_redirect_keeps_headers
func TestRedirectHeaderStripperSameOriginRedirectKeepsHeaders(t *testing.T) {
	h := redirectHop(t, true, map[string]bool{"x-tenant": true}, map[string]string{"Authorization": "Bearer x", "X-Tenant": "t"}, "https://origin.example.test/other")
	if h.Get("Authorization") != "Bearer x" || h.Get("X-Tenant") != "t" {
		t.Fatalf("headers %v", h)
	}
}

// test_mcp_park_log_dedupe.py test_reparked_server_logs_once_not_per_probe
func TestReparkedServerLogsOnceNotPerProbe(t *testing.T) {
	logs := captureLogs(t)
	s, _ := fakeTransportTask(t, "blender", map[string]any{"command": "x"}, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call >= 10 {
			s.shutdownEv.Set()
		}
		return "", errors.New("addon not running")
	})
	writeMCPServers(t, "  blender:\n    command: x\n")
	maxInitialConnectRetries = 1
	parkedRetryInterval = time.Millisecond
	_ = s.start(context.Background(), s.rendered)
	waitUntil(t, "the run loop never ended", func() bool { return taskDone(s) })
	warns, debugs := 0, 0
	for _, r := range logs.all() {
		if !strings.Contains(r.msg, "failed initial connection after") {
			continue
		}
		if r.level >= slog.LevelWarn {
			warns++
		} else if r.level == slog.LevelDebug {
			debugs++
		}
	}
	if warns+debugs < 4 {
		t.Fatalf("scenario only produced %d park lines, expected several self-probe re-park cycles", warns+debugs)
	}
	if warns != 1 || debugs < 3 {
		t.Fatalf("the first park must warn exactly once and the re-parks must be DEBUG, warns %d debugs %d", warns, debugs)
	}
}

// test_mcp_parked_probe_no_browser.py test_explicit_reconnect_request_keeps_the_callers_interactivity
func TestExplicitReconnectRequestKeepsTheCallersInteractivity(t *testing.T) {
	s := newServerTask("srv", nil)
	ctx := context.Background()
	s.reconnect.Set()
	if got := s.waitForReconnectOrShutdown(ctx, time.Second); got != "reconnect" {
		t.Fatalf("explicit request read as %q", got)
	}
	if got := s.waitForReconnectOrShutdown(ctx, 10*time.Millisecond); got != "self-probe" {
		t.Fatalf("timed wake read as %q", got)
	}
	s.shutdownEv.Set()
	if got := s.waitForReconnectOrShutdown(ctx, 10*time.Millisecond); got != "shutdown" {
		t.Fatalf("shutdown read as %q", got)
	}
}

// test_mcp_parked_self_probe.py test_revival_discovery_registers_tools_while_ready_is_cleared
func TestRevivalDiscoveryRegistersToolsWhileReadyIsCleared(t *testing.T) {
	resetLedgers(t)
	resetHandlerLedgers(t)
	tr := &handlerTransport{in: make(chan *Message, 16), respond: func(method string, params map[string]any) (any, error) {
		if method == methodToolsList {
			return toolPage([]string{"send_message"}, ""), nil
		}
		return map[string]any{}, nil
	}}
	s := newServerTask("srv", &tools.Env{})
	sess := NewSession(tr, SessionOptions{Name: "srv"})
	t.Cleanup(func() { sess.Close() })
	s.mu.Lock()
	s.session = sess
	s.config = map[string]any{"url": "https://example.test/mcp"}
	s.mu.Unlock()
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "srv"}] = s
	mcpMu.Unlock()
	if s.ready.IsSet() {
		t.Fatal("readiness must be clear for a revival discovery")
	}
	if err := s.discoverTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if names := s.registeredToolNames(); !slices.Contains(names, "mcp__srv__send_message") {
		t.Fatalf("revival registered %v", names)
	}
}

// test_mcp_parked_self_probe.py test_parked_server_self_probes_and_revives
func TestParkedServerSelfProbesAndRevives(t *testing.T) {
	var backendUp atomic.Bool
	var revived atomic.Int32
	s, calls := fakeTransportTask(t, "srv", map[string]any{"command": "x"}, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call == 1 {
			fakeConnected(s, false)
			return "", errors.New("backend outage begins")
		}
		if !backendUp.Load() {
			return "", errors.New("backend still down")
		}
		fakeConnected(s, false)
		if err := s.registerDiscoveredToolsIfNeeded(ctx); err == nil && len(s.registeredToolNames()) > 0 {
			revived.Add(1)
		}
		select {
		case <-s.reconnect.Wait():
		case <-s.shutdownEv.Wait():
		}
		return s.finishLifecycleWait(), nil
	})
	writeMCPServers(t, "  srv:\n    command: x\n")
	maxReconnectRetries = 1
	parkedRetryInterval = 50 * time.Millisecond
	s.tools = []*Tool{{Name: "tool", InputSchema: map[string]any{"type": "object"}}}
	s.toolNames = []string{"mcp__srv__tool"}
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "srv"}] = s
	mcpMu.Unlock()
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "server never parked", func() bool { return s.parked() && len(s.registeredToolNames()) == 0 })
	if taskDone(s) {
		t.Fatal("run task exited instead of parking")
	}
	backendUp.Store(true)
	waitUntil(t, fmt.Sprintf("parked server never self-probed back to life (transport_calls=%d)", calls.Load()), func() bool { return revived.Load() >= 1 })
	s.shutdown(context.Background())
}

// The KeyboardInterrupt leaf a fatal signal leaves inside a transport group
type KeyboardInterrupt struct{}

func (KeyboardInterrupt) Error() string { return "KeyboardInterrupt" }

// test_mcp_rapid_drop_budget.py TestReconnectGroupFatalSignals test_keyboard_interrupt_leaf_reraises
func TestReconnectGroupFatalSignalsKeyboardInterruptLeafReraises(t *testing.T) {
	s := newServerTask("t", nil)
	s.ready.Set()
	group := errors.Join(errors.New("drop"), KeyboardInterrupt{})
	reason, err := s.reconnectOrReraise(context.Background(), &sessionDropError{err: group})
	if reason == "reconnect" || err != group {
		t.Fatalf("a fatal signal leaf was masked as %q err %v", reason, err)
	}
}

// test_mcp_rapid_drop_budget.py TestReconnectGroupFatalSignals test_plain_transient_drop_still_reconnects
func TestReconnectGroupFatalSignalsPlainTransientDropStillReconnects(t *testing.T) {
	s := newServerTask("t", nil)
	s.ready.Set()
	reason, err := s.reconnectOrReraise(context.Background(), &sessionDropError{err: errors.Join(errors.New("sse stream dropped"))})
	if err != nil || reason != "reconnect" {
		t.Fatalf("reason %q err %v", reason, err)
	}
}
