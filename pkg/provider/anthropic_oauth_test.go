package provider

import (
	"context"
	"sort"
	"strings"
	"testing"
)

func TestAnthropicKnownStreamBreakageFallsBackToCreate(t *testing.T) {
	srv := newAnthServer(t, anthReplySSE(anthSSETextDelta(0, "early"), anthSSEStart()), anthReplyJSON(200, anthMessage("end_turn", anthText("ok"))))
	c := srv.client(srv.route("https://api.minimax.io/anthropic", "k"))
	resp, err := c.Stream(context.Background(), anthUserRequest("MiniMax-M2", "hi"), func(StreamEvent) error { return nil })
	if err != nil || resp.Message.Content != "ok" {
		t.Fatalf("event order breakage must fall back to create: %v %+v", err, resp)
	}
	reqs := srv.requests()
	if len(reqs) != 2 || reqs[0].Body["stream"] != true {
		t.Fatalf("expected one stream then one create, got %d", len(reqs))
	}
	if _, has := reqs[1].Body["stream"]; has {
		t.Fatalf("fallback create must not carry stream: %v", reqs[1].Body)
	}
	if !c.nonStreaming {
		t.Fatal("custom anthropic_messages event order breakage must disable streaming for the session")
	}
}

func TestAnthropicUnrelatedStreamErrorStillRaises(t *testing.T) {
	srv := newAnthServer(t, anthReplySSE(anthSSEStart(), map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "'Foo' object has no attribute 'bar'"}}))
	c := srv.client(srv.route("", "sk-ant-api03-test"))
	if _, err := c.Stream(context.Background(), anthUserRequest("claude-sonnet-4-6", "hi"), func(StreamEvent) error { return nil }); err == nil {
		t.Fatal("unrelated stream error must raise")
	}
	if n := len(srv.requests()); n != 1 || c.nonStreaming {
		t.Fatalf("unrelated error must not fall back to create, sent %d latched %v", n, c.nonStreaming)
	}
}

func TestAnthropicNullUsageStreamIsNormalizedWithoutCreateRetry(t *testing.T) {
	start := anthSSEStart()
	start["message"].(map[string]any)["usage"] = nil
	start["message"].(map[string]any)["model"] = "MiniMax-M2"
	delta := anthSSEMessageDelta("end_turn")
	delta["usage"] = nil
	srv := newAnthServer(t, anthReplySSE(start, anthSSEBlockStart(0, anthText("")), anthSSETextDelta(0, "hi"), anthSSEBlockStop(0), delta, anthSSEStop()))
	c := srv.client(srv.route("https://api.minimax.io/anthropic", "k"))
	resp, err := c.Stream(context.Background(), anthUserRequest("MiniMax-M2", "hi"), func(StreamEvent) error { return nil })
	if err != nil || resp.Message.Content != "hi" || resp.FinishReason != "stop" {
		t.Fatalf("null usage stream: %v %+v", err, resp)
	}
	if n := len(srv.requests()); n != 1 {
		t.Fatalf("null usage must not trigger a create retry, sent %d", n)
	}
}

func anthOAuthNormalizer(t *testing.T, registered ...string) *anthropicClient {
	t.Helper()
	known := map[string]bool{}
	for _, n := range registered {
		known[n] = true
	}
	prev := anthRegisteredTool
	SetRegisteredToolLookup(func(name string) bool { return known[name] })
	t.Cleanup(func() { anthRegisteredTool = prev })
	return &anthropicClient{route: Route{Provider: "anthropic", Model: "claude-sonnet-4"}, transport: newAnthDirectTransport(Route{}), oauthWire: true, requestToolNames: map[string]bool{}}
}

func anthNormalizedToolName(t *testing.T, c *anthropicClient, wire string) string {
	t.Helper()
	resp, err := c.normalizeResponse(anthMessage("end_turn", anthToolUse("tc_1", wire, map[string]any{"query": "test"})), 200, nil, 0)
	if err != nil || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("normalize: %v %+v", err, resp)
	}
	return resp.Message.ToolCalls[0].Function.Name
}

func TestAnthropicMCPPrefixStrip(t *testing.T) {
	c := anthOAuthNormalizer(t, "read_file", "terminal", "web_search")
	if got := anthNormalizedToolName(t, c, "mcp__read_file"); got != "read_file" {
		t.Fatalf("OAuth injected prefix must strip, got %q", got)
	}
	c.oauthWire = false
	if got := anthNormalizedToolName(t, c, "mcp__read_file"); got != "mcp__read_file" {
		t.Fatalf("no strip without the OAuth wire, got %q", got)
	}
}

func TestAnthropicOAuthAliasRoundTrip(t *testing.T) {
	if got := anthNormalizedToolName(t, anthOAuthNormalizer(t, "session_search"), "mcp__chat_history_lookup"); got != "session_search" {
		t.Fatalf("session_search alias round trip: %q", got)
	}
	if got := anthNormalizedToolName(t, anthOAuthNormalizer(t, "memory"), "mcp__context_notes"); got != "memory" {
		t.Fatalf("memory alias round trip: %q", got)
	}
	if got := anthNormalizedToolName(t, anthOAuthNormalizer(t, "mcp_chat_history_lookup", "session_search"), "mcp__chat_history_lookup"); got != "mcp_chat_history_lookup" {
		t.Fatalf("registered tool must win over the alias: %q", got)
	}
}

func anthOAuthBuild(t *testing.T, tools []map[string]any, isOAuth bool, messages []map[string]any, toolChoice any) map[string]any {
	t.Helper()
	if messages == nil {
		messages = anthUserOnly("Hi")
	}
	kw, err := anthBuildKwargs(anthBuildInput{model: "claude-sonnet-4-6", messages: messages, tools: tools, maxTokens: 4096, toolChoice: toolChoice, isOAuth: isOAuth})
	if err != nil {
		t.Fatal(err)
	}
	return kw
}

func anthTool(name, description string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description, "parameters": map[string]any{}}}
}

func anthToolNames(kw map[string]any) []string {
	var names []string
	for _, tool := range anthBlocks(kw["tools"]) {
		names = append(names, asStr(tool["name"]))
	}
	sort.Strings(names)
	return names
}

func anthSystemText(kw map[string]any) string {
	var texts []string
	for _, b := range anthBlocks(kw["system"]) {
		if b["type"] == "text" {
			texts = append(texts, asStr(b["text"]))
		}
	}
	return strings.Join(texts, "\n")
}

func TestAnthropicOAuthNoSingleUnderscoreMCPOnWire(t *testing.T) {
	kw := anthOAuthBuild(t, []map[string]any{anthTool("read_file", "x"), anthTool("mcp_linear_get_issue", "y"), anthTool("terminal", "z")}, true, nil, nil)
	names := anthToolNames(kw)
	if strings.Join(names, ",") != "mcp__linear_get_issue,mcp__read_file,mcp__terminal" {
		t.Fatalf("wire names wrong: %v", names)
	}
}

func TestAnthropicOAuthClassifierAlias(t *testing.T) {
	kw := anthOAuthBuild(t, []map[string]any{anthTool("session_search", "Use session_search to recall prior chats."), anthTool("memory", "Persist notes with memory.")}, true, nil, nil)
	if got := strings.Join(anthToolNames(kw), ","); got != "mcp__chat_history_lookup,mcp__context_notes" {
		t.Fatalf("aliased names wrong: %s", got)
	}

	kw = anthOAuthBuild(t, []map[string]any{anthTool("session_search", "Call session_search to recall prior chats."), anthTool("memory", "Persist notes; uses working memory internally.")}, true, nil, nil)
	desc := map[string]string{}
	for _, tool := range anthBlocks(kw["tools"]) {
		desc[asStr(tool["name"])] = asStr(tool["description"])
	}
	if d := desc["mcp__chat_history_lookup"]; !strings.Contains(d, "chat_history_lookup") || strings.Contains(d, "session_search") {
		t.Fatalf("session_search prose alias wrong: %q", d)
	}
	if !strings.Contains(desc["mcp__context_notes"], "memory") {
		t.Fatalf("memory prose must stay: %q", desc["mcp__context_notes"])
	}

	kw = anthOAuthBuild(t, []map[string]any{anthTool("session_search", "x")}, true, []map[string]any{{"role": "system", "content": "When relevant, use session_search to recall it."}, {"role": "user", "content": "Hi"}}, nil)
	if s := anthSystemText(kw); !strings.Contains(s, "chat_history_lookup") || strings.Contains(s, "session_search") {
		t.Fatalf("system prose alias wrong: %s", s)
	}

	kw = anthOAuthBuild(t, []map[string]any{anthTool("session_search", "x")}, true, []map[string]any{{"role": "system", "content": "See tools/session_search_tool.py for details."}, {"role": "user", "content": "Hi"}}, nil)
	if s := anthSystemText(kw); !strings.Contains(s, "tools/session_search_tool.py") {
		t.Fatalf("longer identifier must survive: %s", s)
	}

	kw = anthOAuthBuild(t, []map[string]any{anthTool("session_search", "x")}, true, nil, "session_search")
	anthMustEqualJSON(t, kw["tool_choice"], map[string]any{"type": "tool", "name": "mcp__chat_history_lookup"})
	if names := anthToolNames(kw); len(names) != 1 || names[0] != "mcp__chat_history_lookup" {
		t.Fatalf("tool_choice must match a wire tool: %v", names)
	}

	kw = anthOAuthBuild(t, []map[string]any{anthTool("read_file", "x")}, true, nil, "read_file")
	anthMustEqualJSON(t, kw["tool_choice"], map[string]any{"type": "tool", "name": "mcp__read_file"})

	kw = anthOAuthBuild(t, []map[string]any{anthTool("session_search", "x"), anthTool("mcp_chat_history_lookup", "x")}, true, nil, nil)
	if got := strings.Join(anthToolNames(kw), ","); got != "mcp__chat_history_lookup,mcp__session_search" {
		t.Fatalf("collision must skip the alias: %s", got)
	}

	kw = anthOAuthBuild(t, []map[string]any{anthTool("session_search", "Use session_search to recall prior chats.")}, false, nil, nil)
	tools := anthBlocks(kw["tools"])
	if tools[0]["name"] != "session_search" || !strings.Contains(asStr(tools[0]["description"]), "session_search") {
		t.Fatalf("api key path must never alias: %v", tools[0])
	}
}

func TestTokenEndpointRejectionCarriesTheBodysOAuthCode(t *testing.T) {
	dead := anthropicOAuthHTTPError(400, []byte(`{"error": "invalid_grant", "error_description": "refresh token revoked"}`), "refresh")
	if dead.Error() != "Anthropic token refresh failed: HTTP 400 invalid_grant (refresh token revoked)" || !dead.ReloginRequired() {
		t.Fatalf("dead grant %q relogin %v", dead.Error(), dead.ReloginRequired())
	}
	opaque := anthropicOAuthHTTPError(502, []byte("<html>bad gateway</html>"), "exchange")
	if opaque.Error() != "Anthropic token exchange failed: HTTP 502 error" || opaque.ReloginRequired() || IsTerminalAnthropicRefreshError(opaque) {
		t.Fatalf("opaque body %q relogin %v", opaque.Error(), opaque.ReloginRequired())
	}
	if !IsTerminalAnthropicRefreshError(dead) {
		t.Fatal("a dead grant read as retryable")
	}
}
