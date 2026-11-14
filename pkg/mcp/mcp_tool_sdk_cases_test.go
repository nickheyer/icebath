package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// True when the registry holds a live task under name
func serverSpawned(name string) bool {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	_, spawned := mcpServers[ServerKey{Name: name}]
	return spawned
}

// test_mcp_tool.py TestHTTPConfig test_is_http_with_url
func TestHTTPConfigIsHttpWithUrl(t *testing.T) {
	isolateHome(t)
	s := newServerTask("remote", nil)
	s.config = map[string]any{"url": "https://example.com/mcp"}
	if !s.isHTTP() {
		t.Fatal("a url config did not read as http")
	}
}

// test_mcp_server_log_notifications.py TestLogLevelMap test_all_mcp_levels_mapped
func TestLogLevelMapAllMcpLevelsMapped(t *testing.T) {
	isolateHome(t)
	for _, lvl := range []string{"debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"} {
		if _, ok := logLevelMap[lvl]; !ok {
			t.Fatalf("%s unmapped", lvl)
		}
	}
}

// Session named like the Python MCPServerTask whose logging callback the cases drive
func logSession(name string) *Session {
	return &Session{opts: SessionOptions{Name: name}}
}

// test_mcp_server_log_notifications.py TestLoggingCallback test_routes_to_hermes_logger_with_server_tag
func TestLoggingCallbackRoutesToHermesLoggerWithServerTag(t *testing.T) {
	isolateHome(t)
	logs := captureLogs(t)
	logSession("log_srv").onServerLog(json.RawMessage(`{"level":"info","data":"server started","logger":null}`))
	for _, r := range logs.all() {
		if strings.Contains(r.msg, "log_srv") && strings.Contains(r.msg, "server started") && r.level == slog.LevelInfo {
			return
		}
	}
	t.Fatalf("records %s", logs.text())
}

// test_mcp_server_log_notifications.py TestLoggingCallback test_includes_sub_logger_name
func TestLoggingCallbackIncludesSubLoggerName(t *testing.T) {
	isolateHome(t)
	logs := captureLogs(t)
	logSession("log_srv").onServerLog(json.RawMessage(`{"level":"warning","data":"rate limited","logger":"http"}`))
	for _, r := range logs.all() {
		if strings.Contains(r.msg, "log_srv/http") && strings.Contains(r.msg, "rate limited") && r.level == slog.LevelWarn {
			return
		}
	}
	t.Fatalf("records %s", logs.text())
}

// test_mcp_server_log_notifications.py TestLoggingCallback test_handler_never_raises
func TestLoggingCallbackHandlerNeverRaises(t *testing.T) {
	isolateHome(t)
	captureLogs(t)
	s := logSession("log_srv")
	for _, raw := range []string{`{}`, `null`, `[]`, `not json`, `{"level":7,"data":{"k":[1,2]},"logger":["x"]}`} {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("%s panicked: %v", raw, p)
				}
			}()
			s.onServerLog(json.RawMessage(raw))
		}()
	}
}

// test_mcp_server_log_notifications.py TestSDKSupportGate test_current_sdk_supports_logging_callback
func TestSDKSupportGateCurrentSdkSupportsLoggingCallback(t *testing.T) {
	isolateHome(t)
	logs := captureLogs(t)
	logSession("gate").onServerLog(json.RawMessage(`{"level":"info","data":"probe"}`))
	if got := logs.count("MCP server log [gate]: probe"); got != 1 {
		t.Fatalf("server log routed %d times", got)
	}
}

// Session over a scripted transport whose tools/list answers come from page, keyed by cursor and call number
func pagingSession(t *testing.T, name string, page func(cursor string, call int) (any, error)) (*Session, *handlerTransport) {
	t.Helper()
	var calls atomic.Int32
	tr := &handlerTransport{in: make(chan *Message, 16), respond: func(method string, params map[string]any) (any, error) {
		if method != methodToolsList {
			return map[string]any{}, nil
		}
		cursor, _ := params["cursor"].(string)
		return page(cursor, int(calls.Add(1)))
	}}
	sess := NewSession(tr, SessionOptions{Name: name})
	t.Cleanup(func() { sess.Close() })
	return sess, tr
}

func toolNames(tools []*Tool) []string {
	var out []string
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	return out
}

func toolPage(names []string, next string) map[string]any {
	var items []any
	for _, n := range names {
		items = append(items, map[string]any{"name": n})
	}
	page := map[string]any{"tools": items}
	if next != "" {
		page["nextCursor"] = next
	}
	return page
}

// test_mcp_list_pagination.py TestPaginateFullList test_single_page_no_cursor
func TestPaginateFullListSinglePageNoCursor(t *testing.T) {
	isolateHome(t)
	var mu sync.Mutex
	var cursors []string
	sess, tr := pagingSession(t, "srv", func(cursor string, call int) (any, error) {
		mu.Lock()
		cursors = append(cursors, cursor)
		mu.Unlock()
		return toolPage([]string{"a", "b"}, ""), nil
	})
	items, err := paginate(context.Background(), sess, methodToolsList, "tools", func(p *listPage) []*Tool { return p.Tools }, nil)
	if err != nil || strings.Join(toolNames(items), ",") != "a,b" {
		t.Fatalf("items %v err %v", toolNames(items), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if tr.count(methodToolsList) != 1 || len(cursors) != 1 || cursors[0] != "" {
		t.Fatalf("list called %d times with cursors %q", tr.count(methodToolsList), cursors)
	}
}

// test_mcp_list_pagination.py TestPaginateFullList test_runaway_cursor_capped
func TestPaginateFullListRunawayCursorCapped(t *testing.T) {
	isolateHome(t)
	logs := captureLogs(t)
	sess, tr := pagingSession(t, "srv", func(cursor string, call int) (any, error) {
		return toolPage([]string{fmt.Sprintf("t%d", call)}, fmt.Sprintf("c%d", call)), nil
	})
	items, err := sess.ListTools(context.Background(), nil)
	if err != nil || len(items) != listMaxPages || tr.count(methodToolsList) != listMaxPages {
		t.Fatalf("items %d calls %d err %v", len(items), tr.count(methodToolsList), err)
	}
	if logs.count("pagination exceeded") != 1 {
		t.Fatalf("truncation warning missing: %s", logs.text())
	}
}

// test_mcp_list_pagination.py TestDiscoveryUsesPagination test_discover_tools_drains_all_pages
func TestDiscoveryUsesPaginationDiscoverToolsDrainsAllPages(t *testing.T) {
	isolateHome(t)
	pages := map[string]any{"": toolPage([]string{"first"}, "page-2"), "page-2": toolPage([]string{"second"}, "")}
	srv, _ := handlerServer(t, "pag_srv", func(method string, params map[string]any) (any, error) {
		if method != methodToolsList {
			return map[string]any{}, nil
		}
		cursor, _ := params["cursor"].(string)
		return pages[cursor], nil
	})
	srv.mu.Lock()
	srv.config = map[string]any{"command": "test"}
	srv.mu.Unlock()
	if err := srv.discoverTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	names := toolNames(srv.tools)
	srv.mu.Unlock()
	if strings.Join(names, ",") != "first,second" {
		t.Fatalf("discovered %v", names)
	}
}

// test_mcp_list_pagination.py TestTypeErrorPropagation test_type_error_from_modern_list_call_propagates
func TestTypeErrorPropagationTypeErrorFromModernListCallPropagates(t *testing.T) {
	isolateHome(t)
	sess, tr := pagingSession(t, "srv", func(cursor string, call int) (any, error) {
		if call == 1 {
			return toolPage([]string{"first"}, "page-2"), nil
		}
		return nil, errors.New("server response decode failed")
	})
	_, err := sess.ListTools(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "server response decode failed") {
		t.Fatalf("err %v", err)
	}
	if n := tr.count(methodToolsList); n != 2 {
		t.Fatalf("the legacy-cursor retry must not run, list called %d times", n)
	}
}

// test_mcp_list_pagination.py TestTypeErrorPropagation test_legacy_signature_still_uses_cursor_fallback
func TestTypeErrorPropagationLegacySignatureStillUsesCursorFallback(t *testing.T) {
	isolateHome(t)
	sess, tr := pagingSession(t, "srv", func(cursor string, call int) (any, error) {
		if call == 1 {
			return toolPage([]string{"first"}, "p2"), nil
		}
		if cursor != "p2" {
			return nil, fmt.Errorf("unexpected cursor %q", cursor)
		}
		return map[string]any{"tools": []any{map[string]any{"name": "second"}}, "nextCursor": nil}, nil
	})
	items, err := sess.ListTools(context.Background(), nil)
	if err != nil || strings.Join(toolNames(items), ",") != "first,second" || tr.count(methodToolsList) != 2 {
		t.Fatalf("items %v calls %d err %v", toolNames(items), tr.count(methodToolsList), err)
	}
}

// The _task_with_pids helper, a task tracking pids over a stdio or http config
func taskWithPids(pids []int, http bool) *serverTask {
	s := newServerTask("x", nil)
	s.stdioChildPIDs = pids
	s.config = map[string]any{"command": "x"}
	if http {
		s.config = map[string]any{"url": "http://example.invalid"}
	}
	return s
}

// Replaces the child pid probe for one case, nil meaning no probe support, and counts its calls
func fakePidProbe(t *testing.T, probe func(pid int) (bool, error)) *atomic.Int32 {
	t.Helper()
	prev := childPidProbe
	var calls atomic.Int32
	childPidProbe = nil
	if probe != nil {
		childPidProbe = func(pid int) (bool, error) {
			calls.Add(1)
			return probe(pid)
		}
	}
	t.Cleanup(func() { childPidProbe = prev })
	return &calls
}

// test_mcp_stdio_children_dead.py test_live_child_reports_not_dead
func TestLiveChildReportsNotDead(t *testing.T) {
	isolateHome(t)
	fakePidProbe(t, func(int) (bool, error) { return true, nil })
	if taskWithPids([]int{60634}, false).stdioChildrenDead() {
		t.Fatal("an alive tracked pid reported all dead")
	}
}

// test_mcp_stdio_children_dead.py test_all_children_dead_reports_dead
func TestAllChildrenDeadReportsDead(t *testing.T) {
	isolateHome(t)
	fakePidProbe(t, func(int) (bool, error) { return false, nil })
	if !taskWithPids([]int{111, 222}, false).stdioChildrenDead() {
		t.Fatal("every child gone did not report dead")
	}
}

// test_mcp_stdio_children_dead.py test_mixed_liveness_reports_not_dead
func TestMixedLivenessReportsNotDead(t *testing.T) {
	isolateHome(t)
	fakePidProbe(t, func(pid int) (bool, error) { return pid != 111, nil })
	if taskWithPids([]int{111, 222}, false).stdioChildrenDead() {
		t.Fatal("one live sibling did not keep the verdict alive")
	}
}

// test_mcp_stdio_children_dead.py test_no_captured_pids_stays_fail_open
func TestNoCapturedPidsStaysFailOpen(t *testing.T) {
	isolateHome(t)
	calls := fakePidProbe(t, func(int) (bool, error) { return false, nil })
	if taskWithPids(nil, false).stdioChildrenDead() || taskWithPids([]int{1}, true).stdioChildrenDead() || calls.Load() != 0 {
		t.Fatalf("unknown liveness failed fast, probes %d", calls.Load())
	}
}

// test_mcp_stdio_children_dead.py test_psutil_unavailable_stays_fail_open
func TestPsutilUnavailableStaysFailOpen(t *testing.T) {
	isolateHome(t)
	fakePidProbe(t, nil)
	if taskWithPids([]int{1}, false).stdioChildrenDead() {
		t.Fatal("missing probe support read as proof of child death")
	}
}

// test_mcp_stdio_children_dead.py test_pid_probe_error_stays_fail_open
func TestPidProbeErrorStaysFailOpen(t *testing.T) {
	isolateHome(t)
	fakePidProbe(t, func(int) (bool, error) { return false, errors.New("probe failed") })
	if taskWithPids([]int{1}, false).stdioChildrenDead() {
		t.Fatal("a failed probe authorized the destructive fast-fail")
	}
}

// test_mcp_stdio_children_dead.py test_watcher_does_not_resolve_while_a_child_is_alive
func TestWatcherDoesNotResolveWhileAChildIsAlive(t *testing.T) {
	isolateHome(t)
	fakePidProbe(t, func(int) (bool, error) { return true, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if taskWithPids([]int{60634}, false).watchStdioChildren(ctx) {
		t.Fatal("the watcher resolved while a child was still live")
	}
}

// test_mcp_stdio_children_dead.py test_watcher_resolves_when_all_children_are_dead
func TestWatcherResolvesWhenAllChildrenAreDead(t *testing.T) {
	isolateHome(t)
	fakePidProbe(t, func(int) (bool, error) { return false, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if !taskWithPids([]int{111, 222}, false).watchStdioChildren(ctx) {
		t.Fatal("the watcher did not resolve on the all-dead verdict")
	}
}

// test_mcp_stdio_children_dead.py test_fast_fail_probe_never_calls_the_watcher_on_the_plain_path
func TestFastFailProbeNeverCallsTheWatcherOnThePlainPath(t *testing.T) {
	isolateHome(t)
	srv, tr := handlerServer(t, "srv", func(method string, params map[string]any) (any, error) {
		if method == methodToolsCall {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}, nil
		}
		return map[string]any{}, nil
	})
	calls := fakePidProbe(t, func(int) (bool, error) { return true, nil })
	before := runtime.NumGoroutine()
	res, err := callToolRacingStdioDeath(context.Background(), srv, "srv", "tool", map[string]any{}, nil)
	if err != nil || res == nil || len(res.Content) != 1 || res.Content[0].Text != "ok" || tr.count(methodToolsCall) != 1 {
		t.Fatalf("result %+v err %v", res, err)
	}
	if calls.Load() != 0 {
		t.Fatal("fast-fail probe called the watcher (leaks a coroutine per call)")
	}
	waitUntil(t, "the racing watcher goroutine outlived the call", func() bool { return runtime.NumGoroutine() <= before })
}

// Runs runHTTP against r until the handshake is live, then shuts the task down and returns the run outcome
func runHTTPUntilConnected(t *testing.T, s *serverTask, r *Rendered) (string, error) {
	t.Helper()
	s.config = r.Config
	type outcome struct {
		reason string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		reason, err := s.runHTTP(context.Background(), r)
		done <- outcome{reason, err}
	}()
	deadline := time.After(10 * time.Second)
	for s.currentSession() == nil {
		select {
		case o := <-done:
			return o.reason, o.err
		case <-deadline:
			t.Fatal("the handshake never completed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	s.shutdownEv.Set()
	select {
	case o := <-done:
		return o.reason, o.err
	case <-time.After(10 * time.Second):
		t.Fatal("the run never ended after shutdown")
	}
	return "", nil
}

// test_mcp_streamable_http_arity.py test_run_http_accepts_the_arity_each_sdk_generation_yields
func TestRunHttpAcceptsTheArityEachSdkGenerationYields(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	var posts atomic.Int64
	streamable := jsonMCPServer(t, &posts)
	sse := legacySSEServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) })
	for _, tc := range []struct {
		name string
		cfg  map[string]any
	}{
		{"streamable http", map[string]any{"url": streamable.URL}},
		{"sse", map[string]any{"url": sse.URL + "/mcp", "transport": "sse"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServerTask("remote", nil)
			reason, err := runHTTPUntilConnected(t, s, &Rendered{Config: tc.cfg})
			s.mu.Lock()
			serr := s.err
			s.mu.Unlock()
			if err != nil || reason != "shutdown" || serr != nil {
				t.Fatalf("handshake never completed on %s: reason %q err %v task err %v", tc.name, reason, err, serr)
			}
		})
	}
}

// test_mcp_streamable_http_arity.py test_the_session_streams_are_the_first_two_yielded
func TestTheSessionStreamsAreTheFirstTwoYielded(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	s := newServerTask("remote", nil)
	s.config = map[string]any{"url": "https://example.com/mcp"}
	tr := &handlerTransport{in: make(chan *Message, 16), respond: func(method string, params map[string]any) (any, error) {
		switch method {
		case methodInitialize:
			return InitializeResult{ProtocolVersion: LatestHandshakeVersion, Capabilities: &ServerCapabilities{Tools: map[string]any{}}, ServerInfo: Implementation{Name: "fake", Version: "1"}}, nil
		case methodToolsList:
			return map[string]any{"tools": []any{}}, nil
		}
		return map[string]any{}, nil
	}}
	done := make(chan error, 1)
	go func() {
		_, err := s.serveTransport(context.Background(), func() (Transport, error) { return tr, nil }, "HTTP", 5)
		done <- err
	}()
	waitUntil(t, "the session never came up over the yielded transport", func() bool { return s.currentSession() != nil })
	sess := s.currentSession()
	s.shutdownEv.Set()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if sess.t != tr {
		t.Fatalf("the session was built over %T, not the yielded transport", sess.t)
	}
}

// test_mcp_streamable_http_arity.py test_the_seeded_protocol_header_matches_the_handshake_the_client_sends
func TestTheSeededProtocolHeaderMatchesTheHandshakeTheClientSends(t *testing.T) {
	isolateHome(t)
	tr, err := StartStreamableHTTP(HTTPOptions{URL: "https://example.com/mcp", SSLVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if tr.protocol != LatestHandshakeVersion || (&Session{}).initializeParams().ProtocolVersion != LatestHandshakeVersion {
		t.Fatalf("seeded header %q handshake %q", tr.protocol, (&Session{}).initializeParams().ProtocolVersion)
	}
}

// Streamable HTTP fake recording the protocol version header of every POST
func protocolHeaderRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	var posts atomic.Int64
	inner := jsonMCPHandler(&posts)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Mcp-Protocol-Version"))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// test_mcp_streamable_http_arity.py test_the_seeded_header_is_the_handshake_version_on_the_wire
func TestTheSeededHeaderIsTheHandshakeVersionOnTheWire(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	srv, seen := protocolHeaderRecorder(t)
	s := newServerTask("remote", nil)
	if _, err := runHTTPUntilConnected(t, s, &Rendered{Config: map[string]any{"url": srv.URL}}); err != nil {
		t.Fatal(err)
	}
	if headers := seen(); len(headers) == 0 || headers[0] != LatestHandshakeVersion {
		t.Fatalf("protocol headers on the wire %v", headers)
	}
}

// test_mcp_streamable_http_arity.py test_an_explicit_protocol_header_still_wins
func TestAnExplicitProtocolHeaderStillWins(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	srv, seen := protocolHeaderRecorder(t)
	s := newServerTask("remote", nil)
	cfg := map[string]any{"url": srv.URL, "headers": map[string]any{"MCP-Protocol-Version": "2025-06-18"}}
	if _, err := runHTTPUntilConnected(t, s, &Rendered{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if headers := seen(); len(headers) == 0 || headers[0] != "2025-06-18" {
		t.Fatalf("protocol headers on the wire %v", headers)
	}
}
