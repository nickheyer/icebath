package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// The _caps helper, an initialize result advertising the given capability families
func capsResult(toolsCap, prompts, resources bool) *InitializeResult {
	c := &ServerCapabilities{}
	if toolsCap {
		c.Tools = map[string]any{}
	}
	if prompts {
		c.Prompts = map[string]any{}
	}
	if resources {
		c.Resources = map[string]any{}
	}
	return &InitializeResult{Capabilities: c}
}

// Task over a scripted session whose replies come from respond, counting calls per method
func gatingTask(t *testing.T, cfg map[string]any, caps *InitializeResult, respond func(method string) (any, error)) (*serverTask, *handlerTransport) {
	t.Helper()
	isolateHome(t)
	tr := &handlerTransport{in: make(chan *Message, 32), respond: func(method string, params map[string]any) (any, error) { return respond(method) }}
	sess := NewSession(tr, SessionOptions{Name: "test"})
	t.Cleanup(func() { sess.Close() })
	s := newServerTask("test", &tools.Env{})
	s.mu.Lock()
	s.session = sess
	s.config = cfg
	s.initializeResult = caps
	s.mu.Unlock()
	return s, tr
}

func okReplies(method string) (any, error) {
	if method == methodToolsList {
		return map[string]any{"tools": []any{}}, nil
	}
	return map[string]any{}, nil
}

// Replies of a probe server, ping and tools list each answering from their own script
func probeReplies(ping, list func() (any, error)) func(string) (any, error) {
	return func(method string) (any, error) {
		switch method {
		case methodPing:
			return ping()
		case methodToolsList:
			return list()
		}
		return map[string]any{}, nil
	}
}

func okPing() (any, error) { return map[string]any{}, nil }

func okList() (any, error) { return map[string]any{"tools": []any{}}, nil }

func failWith(err error) func() (any, error) {
	return func() (any, error) { return nil, err }
}

func (s *serverTask) pingUnsupportedFlag() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pingUnsupported
}

// test_mcp_capability_gating.py TestAdvertisesTools test_true_when_tools_capability_present
func TestAdvertisesToolsTrueWhenToolsCapabilityPresent(t *testing.T) {
	s := newServerTask("test", nil)
	s.initializeResult = &InitializeResult{Capabilities: &ServerCapabilities{Tools: map[string]any{"listChanged": true}}}
	if !s.advertisesTools() {
		t.Fatal("a tools capability did not advertise tools")
	}
}

// test_mcp_capability_gating.py TestAdvertisesTools test_legacy_fallback_no_capabilities_attr
func TestAdvertisesToolsLegacyFallbackNoCapabilitiesAttr(t *testing.T) {
	s := newServerTask("test", nil)
	s.initializeResult = &InitializeResult{}
	if !s.advertisesTools() {
		t.Fatal("an initialize result without capabilities must fall back to advertising tools")
	}
}

// test_mcp_capability_gating.py TestDiscoverToolsGating test_skips_list_tools_for_prompt_only_server
func TestDiscoverToolsGatingSkipsListToolsForPromptOnlyServer(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "x"}, capsResult(false, true, false), okReplies)
	s.tools = []*Tool{{Name: "stale"}}
	if err := s.discoverTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	n := len(s.tools)
	s.mu.Unlock()
	if tr.count(methodToolsList) != 0 || n != 0 {
		t.Fatalf("tools/list calls %d stale tools %d", tr.count(methodToolsList), n)
	}
}

// test_mcp_capability_gating.py TestDiscoverToolsGating test_legacy_fallback_still_calls_list_tools
func TestDiscoverToolsGatingLegacyFallbackStillCallsListTools(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "x"}, nil, okReplies)
	if err := s.discoverTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.count(methodToolsList) != 1 {
		t.Fatalf("tools/list calls %d", tr.count(methodToolsList))
	}
}

// test_mcp_capability_gating.py TestRefreshToolsGating test_refresh_noop_for_prompt_only_server
func TestRefreshToolsGatingRefreshNoopForPromptOnlyServer(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "x"}, capsResult(false, true, false), okReplies)
	if err := s.refreshTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.count(methodToolsList) != 0 {
		t.Fatalf("tools/list calls %d", tr.count(methodToolsList))
	}
}

// Drives waitForLifecycleEvent through one timer wake then shutdown, returning the reason and every armed timeout
func runLifecycleCycles(t *testing.T, s *serverTask, advance func(time.Duration)) (string, []time.Duration) {
	t.Helper()
	var mu sync.Mutex
	var timeouts []time.Duration
	prev := lifecycleTimer
	lifecycleTimer = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		timeouts = append(timeouts, d)
		n := len(timeouts)
		mu.Unlock()
		if n > 1 {
			s.shutdownEv.Set()
			return nil
		}
		if advance != nil && d > 0 {
			advance(d)
		}
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	t.Cleanup(func() { lifecycleTimer = prev })
	reason, err := s.waitForLifecycleEvent(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return reason, append([]time.Duration(nil), timeouts...)
}

// test_mcp_capability_gating.py TestKeepaliveProbe test_keepalive_uses_ping_for_prompt_only_server
func TestKeepaliveProbeKeepaliveUsesPingForPromptOnlyServer(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"url": "https://example.test/mcp"}, capsResult(false, true, false), okReplies)
	reason, _ := runLifecycleCycles(t, s, nil)
	if reason != "shutdown" || tr.count(methodPing) != 1 || tr.count(methodToolsList) != 0 {
		t.Fatalf("reason %q pings %d lists %d", reason, tr.count(methodPing), tr.count(methodToolsList))
	}
}

// test_mcp_capability_gating.py TestKeepaliveProbe test_keepalive_uses_ping_legacy_fallback
func TestKeepaliveProbeKeepaliveUsesPingLegacyFallback(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"url": "https://example.test/mcp"}, nil, okReplies)
	reason, _ := runLifecycleCycles(t, s, nil)
	if reason != "shutdown" || tr.count(methodPing) != 1 || tr.count(methodToolsList) != 0 {
		t.Fatalf("reason %q pings %d lists %d", reason, tr.count(methodPing), tr.count(methodToolsList))
	}
}

// test_mcp_capability_gating.py TestKeepaliveInterval test_default_interval_when_unset
func TestKeepaliveIntervalDefaultIntervalWhenUnset(t *testing.T) {
	s, _ := gatingTask(t, map[string]any{"url": "https://example.test/mcp"}, nil, okReplies)
	if _, timeouts := runLifecycleCycles(t, s, nil); len(timeouts) == 0 || timeouts[0] != keepaliveDefault {
		t.Fatalf("remote timeouts %v", timeouts)
	}
	s, _ = gatingTask(t, map[string]any{"command": "example-mcp", "keepalive_interval": 42}, nil, okReplies)
	if _, timeouts := runLifecycleCycles(t, s, nil); len(timeouts) == 0 || timeouts[0] != 42*time.Second {
		t.Fatalf("explicit stdio timeouts %v", timeouts)
	}
}

// test_mcp_capability_gating.py TestKeepaliveInterval test_interval_clamped_to_floor
func TestKeepaliveIntervalIntervalClampedToFloor(t *testing.T) {
	s, _ := gatingTask(t, map[string]any{"url": "https://example.test/mcp", "keepalive_interval": 0.1}, nil, okReplies)
	if _, timeouts := runLifecycleCycles(t, s, nil); len(timeouts) == 0 || timeouts[0] != keepaliveFloor {
		t.Fatalf("timeouts %v", timeouts)
	}
}

// test_mcp_capability_gating.py TestKeepaliveInterval test_stdio_recycle_wakes_after_active_rpc
func TestKeepaliveIntervalStdioRecycleWakesAfterActiveRpc(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "example-mcp"}, nil, okReplies)
	idle := 0.01
	s.mu.Lock()
	s.idleTimeout = &idle
	s.lastToolCallAt = s.lifecycleStartedAt.Add(-time.Second)
	s.mu.Unlock()
	if err := s.rpc.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		reason, _ := s.waitForLifecycleEvent(context.Background(), nil)
		done <- reason
	}()
	select {
	case r := <-done:
		t.Fatalf("lifecycle wait ended with %q while the rpc lock was held", r)
	case <-time.After(100 * time.Millisecond):
	}
	s.rpc.Unlock()
	select {
	case r := <-done:
		if r != "recycle" {
			t.Fatalf("reason %q", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle wait never woke after the rpc lock was released")
	}
	if tr.count(methodPing) != 0 {
		t.Fatal("a ping went out on the recycle wake")
	}
}

// test_mcp_capability_gating.py TestMethodNotFoundDetection test_structural_code_match
func TestMethodNotFoundDetectionStructuralCodeMatch(t *testing.T) {
	if !isMethodNotFoundError(&RPCError{Code: -32601, Message: "boom"}) {
		t.Fatal("the -32601 code was not classified as method not found")
	}
}

// test_mcp_capability_gating.py TestMethodNotFoundDetection test_unrelated_exception_is_not_match
func TestMethodNotFoundDetectionUnrelatedExceptionIsNotMatch(t *testing.T) {
	if isMethodNotFoundError(context.DeadlineExceeded) || isMethodNotFoundError(errors.New("session terminated")) {
		t.Fatal("an unrelated error classified as method not found")
	}
}

// test_mcp_capability_gating.py TestKeepaliveProbeFallback test_uses_ping_when_supported
func TestKeepaliveProbeFallbackUsesPingWhenSupported(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "x"}, capsResult(true, false, false), probeReplies(okPing, okList))
	if err := s.keepaliveProbe(context.Background()); err != nil || tr.count(methodPing) != 1 || tr.count(methodToolsList) != 0 || s.pingUnsupportedFlag() {
		t.Fatalf("err %v pings %d lists %d unsupported %v", err, tr.count(methodPing), tr.count(methodToolsList), s.pingUnsupportedFlag())
	}
}

// test_mcp_capability_gating.py TestKeepaliveProbeFallback test_falls_back_on_unknown_method_string
func TestKeepaliveProbeFallbackFallsBackOnUnknownMethodString(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "x"}, capsResult(true, false, false), probeReplies(failWith(errors.New("Unknown method: ping")), okList))
	if err := s.keepaliveProbe(context.Background()); err != nil || tr.count(methodPing) != 1 || tr.count(methodToolsList) != 1 || !s.pingUnsupportedFlag() {
		t.Fatalf("err %v pings %d lists %d unsupported %v", err, tr.count(methodPing), tr.count(methodToolsList), s.pingUnsupportedFlag())
	}
}

// test_mcp_capability_gating.py TestKeepaliveProbeFallback test_real_liveness_failure_propagates_not_swallowed
func TestKeepaliveProbeFallbackRealLivenessFailurePropagatesNotSwallowed(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "x"}, capsResult(true, false, false), probeReplies(failWith(errors.New("session terminated")), okList))
	err := s.keepaliveProbe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "session terminated") || tr.count(methodToolsList) != 0 || s.pingUnsupportedFlag() {
		t.Fatalf("err %v lists %d unsupported %v", err, tr.count(methodToolsList), s.pingUnsupportedFlag())
	}
}

// test_mcp_capability_gating.py TestKeepaliveProbeFallback test_no_ping_no_tools_propagates_method_not_found
func TestKeepaliveProbeFallbackNoPingNoToolsPropagatesMethodNotFound(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "x"}, capsResult(false, true, false), probeReplies(failWith(&RPCError{Code: -32601, Message: "Method not found"}), okList))
	if err := s.keepaliveProbe(context.Background()); err == nil || tr.count(methodToolsList) != 0 {
		t.Fatalf("err %v lists %d", err, tr.count(methodToolsList))
	}
}

// test_mcp_capability_gating.py TestKeepaliveProbeFallback test_discover_resets_latch
func TestKeepaliveProbeFallbackDiscoverResetsLatch(t *testing.T) {
	s, _ := gatingTask(t, map[string]any{"command": "x"}, capsResult(true, false, false), okReplies)
	s.mu.Lock()
	s.pingUnsupported = true
	s.mu.Unlock()
	if err := s.discoverTools(context.Background()); err != nil || s.pingUnsupportedFlag() {
		t.Fatalf("err %v unsupported %v", err, s.pingUnsupportedFlag())
	}
}

// test_mcp_capability_gating.py TestKeepaliveProbeFallback test_silent_ping_drop_falls_back_to_list_tools
func TestKeepaliveProbeFallbackSilentPingDropFallsBackToListTools(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "x"}, capsResult(true, false, false), probeReplies(failWith(context.DeadlineExceeded), okList))
	if err := s.keepaliveProbe(context.Background()); err != nil || tr.count(methodPing) != 1 || tr.count(methodToolsList) != 1 || !s.pingUnsupportedFlag() {
		t.Fatalf("err %v pings %d lists %d unsupported %v", err, tr.count(methodPing), tr.count(methodToolsList), s.pingUnsupportedFlag())
	}
}

// test_mcp_capability_gating.py TestKeepaliveProbeFallback test_silent_ping_drop_both_fail_propagates
func TestKeepaliveProbeFallbackSilentPingDropBothFailPropagates(t *testing.T) {
	s, _ := gatingTask(t, map[string]any{"command": "x"}, capsResult(true, false, false), probeReplies(failWith(context.DeadlineExceeded), failWith(context.DeadlineExceeded)))
	if err := s.keepaliveProbe(context.Background()); err == nil || !isTimeout(err) || s.pingUnsupportedFlag() {
		t.Fatalf("err %v unsupported %v", err, s.pingUnsupportedFlag())
	}
}

// test_mcp_capability_gating.py TestKeepaliveProbeFallback test_silent_ping_drop_no_tools_propagates
func TestKeepaliveProbeFallbackSilentPingDropNoToolsPropagates(t *testing.T) {
	s, tr := gatingTask(t, map[string]any{"command": "x"}, capsResult(false, true, false), probeReplies(failWith(context.DeadlineExceeded), okList))
	if err := s.keepaliveProbe(context.Background()); err == nil || !isTimeout(err) || tr.count(methodToolsList) != 0 || s.pingUnsupportedFlag() {
		t.Fatalf("err %v lists %d unsupported %v", err, tr.count(methodToolsList), s.pingUnsupportedFlag())
	}
}

// Replaces the server clock with one the test advances, restoring the real clock afterwards
func fakeServerClock(t *testing.T) func(time.Duration) {
	t.Helper()
	var mu sync.Mutex
	base, offset := time.Now(), time.Duration(0)
	prev := serverNow
	serverNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return base.Add(offset)
	}
	t.Cleanup(func() { serverNow = prev })
	return func(d time.Duration) {
		mu.Lock()
		offset += d
		mu.Unlock()
	}
}

// test_mcp_capability_gating.py TestKeepaliveInterval test_default_stdio_connection_waits_without_keepalive
func TestKeepaliveIntervalDefaultStdioConnectionWaitsWithoutKeepalive(t *testing.T) {
	advance := fakeServerClock(t)
	s, tr := gatingTask(t, map[string]any{"command": "example-mcp"}, nil, okReplies)
	reason, timeouts := runLifecycleCycles(t, s, advance)
	s.mu.Lock()
	proven := s.sessionProven
	s.mu.Unlock()
	if reason != "shutdown" || len(timeouts) != 2 || timeouts[0] != keepaliveDefault || timeouts[1] >= 0 || !proven || tr.count(methodPing) != 0 {
		t.Fatalf("reason %q timeouts %v proven %v pings %d", reason, timeouts, proven, tr.count(methodPing))
	}
	s, tr = gatingTask(t, map[string]any{"command": "example-mcp"}, nil, okReplies)
	idle := 60.0
	s.mu.Lock()
	s.idleTimeout = &idle
	s.mu.Unlock()
	reason, timeouts = runLifecycleCycles(t, s, advance)
	s.mu.Lock()
	proven = s.sessionProven
	s.mu.Unlock()
	if reason != "recycle" || len(timeouts) == 0 || timeouts[0] != 60*time.Second || proven || tr.count(methodPing) != 0 {
		t.Fatalf("the idle wake must not prove the session: reason %q timeouts %v proven %v pings %d", reason, timeouts, proven, tr.count(methodPing))
	}
}
