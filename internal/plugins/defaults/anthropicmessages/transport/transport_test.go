package transport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	anthropicmessagesconfig "github.com/nickheyer/icebath/internal/plugins/defaults/anthropicmessages/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

func TestEndpointRules(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.anthropic.com":              "https://api.anthropic.com",
		" https://api.anthropic.com/ ":           "https://api.anthropic.com",
		"https://api.commandcode.ai/provider/v1": "https://api.commandcode.ai/provider",
		"https://opencode.ai/zen/v1/":            "https://opencode.ai/zen",
		"https://api.minimax.io/anthropic":       "https://api.minimax.io/anthropic",
		"https://example.test/v1beta":            "https://example.test/v1beta",
	} {
		if got := normalized(base); got != want {
			t.Errorf("normalized(%q) = %q, want %q", base, got, want)
		}
	}
	for base, want := range map[string]bool{
		"https://api.anthropic.com":                   false,
		"https://api.minimax.io/anthropic":            true,
		"https://API.minimaxi.com/anthropic/":         true,
		"https://api.minimax.io/v1":                   false,
		"https://api.commandcode.ai/provider/v1":      true,
		"https://team.palantirfoundry.com/api":        true,
		"https://x.services.ai.azure.com/anthropic":   true,
		"https://opencode.ai/zen":                     false,
		"https://api.lkeap.cloud.tencent.com/plan/an": false,
	} {
		if got := bearer(base); got != want {
			t.Errorf("bearer(%q) = %v, want %v", base, got, want)
		}
	}
}

func TestStreamAddressesAndAuthenticatesTheEndpoint(t *testing.T) {
	for _, variable := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "ANTHROPIC_BASE_URL"} {
		t.Setenv(variable, "")
	}
	var path string
	var header http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, header = r.URL.Path, r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	}))
	defer server.Close()
	cfg := config.New(t.TempDir())
	anthropicmessagesconfig.Register(cfg)
	host := transport.New()
	Register(&host.Registry, cfg)
	request := &provider.Request{Model: "claude-test", Messages: []*sessionsv1.Message{{Role: sessionsv1.Role_ROLE_USER, Text: "hi"}}}
	listen := func(provider.StreamEvent) error { return nil }

	client, err := host.Client(provider.Route{Provider: "relay", BaseURL: server.URL + "/relay/v1", APIKey: "secret", Mode: provider.ModeAnthropicMessages, Headers: map[string]string{"X-Title": "icebath"}})
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	_, err = client.Stream(t.Context(), request, listen)
	failure, ok := errors.AsType[*provider.APIError](err)
	if !ok || failure.StatusCode != 401 || failure.Kind != provider.ErrAuth || failure.Provider != "relay" || failure.Model != "claude-test" {
		t.Fatalf("Stream() error = %v, want a classified 401", err)
	}
	if path != "/relay/v1/messages" {
		t.Fatalf("request path = %q, want a base ending in /v1 to post at /relay/v1/messages", path)
	}
	if header.Get("X-Api-Key") != "secret" || header.Get("Authorization") != "" || header.Get("X-Title") != "icebath" {
		t.Fatalf("headers = %v, want the key as x-api-key and the route headers", header)
	}

	client, _ = host.Client(provider.Route{Provider: "relay", BaseURL: server.URL, APIKey: "token", Auth: provider.AuthOAuth, Mode: provider.ModeAnthropicMessages})
	client.Stream(t.Context(), request, listen)
	if path != "/v1/messages" || header.Get("Authorization") != "Bearer token" || header.Get("X-Api-Key") != "" {
		t.Fatalf("an oauth route went to %s with %v, want a bearer token", path, header)
	}
}

const toolReply = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"TOOL","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func TestSubscriptionTokenCarriesTheClaudeCodeIdentity(t *testing.T) {
	look, run := anthLookPath, anthRunVersion
	anthLookPath = func(name string) (string, error) { return "/opt/claude/" + name, nil }
	anthRunVersion = func(string) (string, error) { return "9.9.9 (Claude Code)\n", nil }
	anthClaudeCodeVersionCache = ""
	t.Cleanup(func() { anthLookPath, anthRunVersion, anthClaudeCodeVersionCache = look, run, "" })

	var path string
	var header http.Header
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"messages"`
		Fallbacks any `json:"fallbacks"`
		MaxTokens int `json:"max_tokens"`
	}
	named := "TOOL"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, header = r.URL.Path, r.Header.Clone()
		body.System, body.Tools, body.Messages, body.Fallbacks = nil, nil, nil, nil
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.Replace(toolReply, "TOOL", named, 1))
	}))
	defer server.Close()
	cfg := config.New(t.TempDir())
	anthropicmessagesconfig.Register(cfg)
	host := transport.New()
	Register(&host.Registry, cfg)
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	request := &provider.Request{Model: "claude-test", Messages: []*sessionsv1.Message{
		{Role: sessionsv1.Role_ROLE_SYSTEM, Text: "You are Icebath Agent by Nicholas W. Heyer, the icebath-agent. Search with session_search."},
		{Role: sessionsv1.Role_ROLE_USER, Text: "hi"},
	}, Tools: []map[string]any{
		{"type": "function", "function": map[string]any{"name": "terminal", "description": "Runs a command, see session_search", "parameters": schema}},
		{"type": "function", "function": map[string]any{"name": "memory", "description": "Keeps notes", "parameters": schema}},
	}}
	listen := func(provider.StreamEvent) error { return nil }

	subscription := provider.Route{Provider: "anthropic", BaseURL: server.URL + "/anthropic.com", APIKey: "sk-ant-oat01-token", Auth: provider.AuthOAuth, Mode: provider.ModeAnthropicMessages}
	client, err := host.Client(subscription)
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	named = "mcp__terminal"
	response, err := client.Stream(t.Context(), request, listen)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	betas := strings.Join(header.Values("Anthropic-Beta"), ",")
	if path != "/anthropic.com/v1/messages" || header.Get("Authorization") != "Bearer sk-ant-oat01-token" || header.Get("X-Api-Key") != "" ||
		header.Get("User-Agent") != "claude-code/9.9.9 (external, cli)" || header.Get("X-App") != "cli" ||
		!strings.Contains(betas, "claude-code-20250219") || !strings.Contains(betas, "oauth-2025-04-20") || strings.Contains(betas, "fallback") {
		t.Fatalf("subscription request went to %s with %v", path, header)
	}
	if len(body.System) != 2 || body.System[0].Text != "You are Claude Code, Anthropic's official CLI for Claude." ||
		body.System[1].Text != "You are Claude Code by Anthropic, the claude-code. Search with chat_history_lookup." {
		t.Errorf("system = %+v, want the Claude Code prefix and the product named as Claude Code", body.System)
	}
	if len(body.Tools) != 2 || body.Tools[0].Name != "mcp__terminal" || body.Tools[0].Description != "Runs a command, see chat_history_lookup" || body.Tools[1].Name != "mcp__context_notes" {
		t.Errorf("tools = %+v, want the wire names of a subscription sign in", body.Tools)
	}
	if body.Fallbacks != nil || body.MaxTokens != 64000 {
		t.Errorf("fallbacks = %v, max_tokens = %d, want no fallbacks and the configured budget", body.Fallbacks, body.MaxTokens)
	}
	calls := response.Message.GetToolCalls()
	if response.FinishReason != "tool_calls" || len(calls) != 1 || calls[0].GetName() != "terminal" || calls[0].GetArguments() != `{"command":"ls"}` {
		t.Fatalf("response = %v %v, want the tool call under its registered name", response.FinishReason, calls)
	}

	request.Messages = append(request.Messages, response.Message, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_TOOL, ToolCallId: "toolu_1", Text: "ok"})
	named = "mcp__context_notes"
	if response, err = client.Stream(t.Context(), request, listen); err != nil {
		t.Fatalf("second Stream() error = %v", err)
	}
	if len(body.Messages) != 3 || len(body.Messages[1].Content) != 1 || body.Messages[1].Content[0].Name != "mcp__terminal" {
		t.Errorf("replayed messages = %+v, want the earlier tool call under its wire name", body.Messages)
	}
	if calls := response.Message.GetToolCalls(); len(calls) != 1 || calls[0].GetName() != "memory" {
		t.Errorf("aliased tool call = %v, want memory", calls)
	}

	keyed, _ := host.Client(provider.Route{Provider: "relay", BaseURL: server.URL, APIKey: "secret", Mode: provider.ModeAnthropicMessages})
	named = "terminal"
	if _, err := keyed.Stream(t.Context(), request, listen); err != nil {
		t.Fatalf("keyed Stream() error = %v", err)
	}
	if header.Get("X-Api-Key") != "secret" || header.Get("X-App") != "" || strings.HasPrefix(header.Get("User-Agent"), "claude-code/") || strings.Contains(strings.Join(header.Values("Anthropic-Beta"), ","), "oauth") {
		t.Errorf("keyed request headers = %v, want none of the subscription identity", header)
	}
	if len(body.System) != 1 || !strings.HasPrefix(body.System[0].Text, "You are Icebath Agent") || len(body.Tools) != 2 || body.Tools[0].Name != "terminal" || body.Tools[1].Name != "memory" {
		t.Errorf("keyed request = %+v %+v, want the prompt and tools untouched", body.System, body.Tools)
	}
	if len(body.Messages) != 3 || len(body.Messages[1].Content) != 1 || body.Messages[1].Content[0].Name != "terminal" {
		t.Errorf("keyed replay = %+v, want the earlier tool call under its registered name", body.Messages)
	}
}

func TestInboundToolNames(t *testing.T) {
	known := map[string]bool{"terminal": true, "mcp_files_read": true, "mcp__server__tool": true}
	for name, want := range map[string]string{
		"terminal":                 "terminal",
		"mcp__terminal":            "terminal",
		"mcp__files_read":          "mcp_files_read",
		"mcp__server__tool":        "mcp__server__tool",
		"mcp__chat_history_lookup": "session_search",
		"mcp__context_notes":       "memory",
		"mcp__unknown":             "mcp__unknown",
	} {
		if got := inboundToolName(name, known); got != want {
			t.Errorf("inboundToolName(%q) = %q, want %q", name, got, want)
		}
	}
	wire := anthOAuthWireNamer([]map[string]any{{"name": "memory"}, {"name": "context_notes"}, {"name": "mcp_files_read"}})
	for name, want := range map[string]string{"memory": "mcp__memory", "context_notes": "mcp__context_notes", "mcp_files_read": "mcp__files_read", "session_search": "mcp__chat_history_lookup"} {
		if got := wire(name); got != want {
			t.Errorf("wire name of %q = %q, want %q", name, got, want)
		}
	}
}
