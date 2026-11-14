package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// test_mcp_tool_client_info.py test_session_kwargs_carry_hermes_client_identity
func TestSessionKwargsCarryHermesClientIdentity(t *testing.T) {
	params := (&Session{}).initializeParams()
	if params.ClientInfo.Name != "icebath-agent" || params.ClientInfo.Version != ClientVersion {
		t.Fatalf("clientInfo %+v", params.ClientInfo)
	}
}

// test_mcp_stdio_encoding_handler.py TestStdioEncodingErrorHandler
func TestStdioEncodingErrorHandlerStdioServerParamsUsesReplaceEncodingHandler(t *testing.T) {
	line := []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"text\":\"a\xffb\xfe\"}}")
	decoded := decodeReplace(line)
	if strings.Count(decoded, "�") != 2 || strings.Contains(decoded, "\xff") {
		t.Fatalf("undecodable bytes not replaced %q", decoded)
	}
	msgs, err := DecodeMessages([]byte(decoded))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("replaced line does not decode %v %v", msgs, err)
	}
}

func summaryLines(logs *captureHandler) []string {
	var out []string
	for _, r := range logs.all() {
		if strings.Contains(r.msg, "tool(s) from") {
			out = append(out, r.msg)
		}
	}
	return out
}

// test_mcp_startup_summary_names_failures.py test_register_summary_names_failed_server_with_reason
func TestRegisterSummaryNamesFailedServerWithReason(t *testing.T) {
	dir, env := managerHome(t)
	fastServerTimings(t)
	shutdownServersFirst(t)
	maxInitialConnectRetries = 1
	prevBackoff := maxBackoffSeconds
	maxBackoffSeconds = 0.1
	t.Cleanup(func() { maxBackoffSeconds = prevBackoff })
	missing := filepath.Join(dir, "no-such-mcp-binary")
	logs := captureLogs(t)
	if _, err := Connect(context.Background(), env, map[string]*Rendered{"ghost": {Config: map[string]any{"command": missing, "connect_timeout": 15}}}); err != nil {
		t.Fatal(err)
	}
	summaries := summaryLines(logs)
	if len(summaries) != 1 || !strings.Contains(summaries[0], "ghost") || !strings.Contains(summaries[0], "no-such-mcp-binary") {
		t.Fatalf("summaries %v", summaries)
	}
}

// test_mcp_startup_summary_names_failures.py test_summary_marks_candidate_skipped_for_cooldown
func TestSummaryMarksCandidateSkippedForCooldown(t *testing.T) {
	_, _ = managerHome(t)
	ctx := context.Background()
	ok := newServerTask("ok", nil)
	ok.toolNames = []string{"t1", "t2"}
	key := serverKey(ctx, "ok", "", true)
	mcpMu.Lock()
	mcpServers[key] = ok
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		delete(mcpServers, key)
		mcpMu.Unlock()
	})
	logs := captureLogs(t)
	logSummary(ctx, "  MCP:", []string{"ok", "cooled"}, 0, 0)
	summaries := summaryLines(logs)
	if len(summaries) != 1 || !strings.Contains(summaries[0], "cooled") || !strings.Contains(summaries[0], "cooldown") {
		t.Fatalf("summaries %v", summaries)
	}
}

// test_startup_latency_regressions.py TestLazyMcpSdk, the package starts inert with no server work before a connect
func TestLazyMcpSdkModuleImportDoesNotImportMcpSdk(t *testing.T) {
	isolateMCPRegistry(t, context.Background())
	mcpMu.Lock()
	live, connecting := len(mcpServers), len(serverConnecting)
	mcpMu.Unlock()
	if live != 0 || connecting != 0 {
		t.Fatalf("package init left %d live and %d connecting servers", live, connecting)
	}
}

// test_startup_latency_regressions.py TestLazyMcpSdk test_lazy_symbol_getattr_resolves_via_ensure
func TestLazyMcpSdkLazySymbolGetattrResolvesViaEnsure(t *testing.T) {
	msgs, err := DecodeMessages([]byte(decodeReplace([]byte(`{"jsonrpc":"2.0","id":7,"result":{}}`))))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("the protocol symbols must resolve without any sdk gate %v %v", msgs, err)
	}
	if (&Session{}).initializeParams().ProtocolVersion != LatestHandshakeVersion {
		t.Fatal("the handshake revision must resolve without any sdk gate")
	}
}

// test_mcp_tool_session_expired.py _is_session_expired_error unit coverage
func TestIsSessionExpiredDetectsInvalidOrExpiredSession(t *testing.T) {
	if !isSessionExpiredError(errors.New("Invalid params: Invalid or expired session")) {
		t.Fatal("the wpcom-mcp message was not classified as an expired session")
	}
}

func TestIsSessionExpiredDetectsExpiredSessionVariant(t *testing.T) {
	if !isSessionExpiredError(errors.New("Session expired")) || !isSessionExpiredError(errors.New("expired session: abc")) {
		t.Fatal("generic expired session phrasings were not classified")
	}
}

func TestIsSessionExpiredDetectsSessionNotFound(t *testing.T) {
	if !isSessionExpiredError(errors.New("session not found")) || !isSessionExpiredError(errors.New("Unknown session: abc123")) {
		t.Fatal("server side session loss phrasings were not classified")
	}
}

// Exception node carrying only its own message with a cause beneath it, the hermes __cause__ chain shape
type causeLayer struct {
	msg   string
	cause error
}

func (l *causeLayer) Error() string { return l.msg }
func (l *causeLayer) Unwrap() error { return l.cause }

func TestIsSessionExpiredTraversalIsBudgetBounded(t *testing.T) {
	var err error = errors.New("leaf")
	for i := 0; i < excTraversalMaxNodes*2; i++ {
		err = &causeLayer{msg: fmt.Sprintf("layer %d", i), cause: err}
	}
	done := make(chan bool, 1)
	go func() { done <- isSessionExpiredError(err) }()
	select {
	case expired := <-done:
		if expired {
			t.Fatal("a chain without a transport signal classified as expired")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the traversal never terminated")
	}
}

func TestIsSessionExpiredWalksGroupChain(t *testing.T) {
	if !isSessionExpiredError(errors.Join(errors.New("unrelated"), errors.New("session terminated"))) {
		t.Fatal("a marker beside the group was not classified")
	}
	if isSessionExpiredError(errors.Join(errors.New("session terminated"), ErrInterrupted)) {
		t.Fatal("an interrupt beside the group must override the marker")
	}
}
