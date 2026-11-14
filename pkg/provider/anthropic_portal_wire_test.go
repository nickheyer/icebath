package provider

import (
	"context"
	"testing"
	"time"
)

const (
	anthPortalURL  = "https://inference-api.nousresearch.com/v1"
	anthStagingURL = "https://ai.wildebeest-newton.ts.net/v1"
)

func TestAnthropicPortalLookalikeHostDoesNotGetPortalTreatment(t *testing.T) {
	spoofed := "https://inference-api.nousresearch.com.attacker.test/v1"
	if isNousPortalEndpoint(spoofed) || requiresBearerAuth(spoofed) {
		t.Fatal("a lookalike host must not get portal treatment")
	}
}

func TestAnthropicPortalJWTAuthenticatesWithBearerNotXAPIKey(t *testing.T) {
	got := anthSendOne(t, anthPortalURL, "portal-invoke-jwt")
	if got.Header.Get("Authorization") != "Bearer portal-invoke-jwt" {
		t.Fatalf("portal must use bearer: %v", got.Header)
	}
	if _, has := got.Header["X-Api-Key"]; has {
		t.Fatalf("bearer construction must omit x-api-key: %v", got.Header)
	}
}

func TestAnthropicPortalBearerDoesNotAlsoSendEnvAnthropicAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	srv := newAnthServer(t)
	route := srv.route(anthPortalURL, "portal-invoke-jwt")
	for _, timeout := range []time.Duration{0, 30 * time.Second} {
		route.Timeout = timeout
		if _, err := srv.client(route).Complete(context.Background(), anthUserRequest("claude-sonnet-4-20250514", "hi")); err != nil {
			t.Fatal(err)
		}
		got := srv.last()
		if _, has := got.Header["X-Api-Key"]; has || got.Header.Get("Authorization") != "Bearer portal-invoke-jwt" {
			t.Fatalf("timeout %v: %v", timeout, got.Header)
		}
	}
}

// Messages kwargs for one user turn on a base URL
func anthPortalKwargs(t *testing.T, model, baseURL string) map[string]any {
	t.Helper()
	kwargs, err := anthBuildKwargs(anthBuildInput{model: model, messages: []map[string]any{{"role": "user", "content": "hi"}}, maxTokens: 1024, baseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	return kwargs
}

func TestAnthropicPortalReceivesTheCatalogIDUnchanged(t *testing.T) {
	for _, model := range []string{"anthropic/claude-opus-4.8", "anthropic/claude-opus-5", "anthropic/claude-haiku-4.5"} {
		if got := anthPortalKwargs(t, model, anthPortalURL)["model"]; got != model {
			t.Errorf("%s became %v", model, got)
		}
	}
	if got := anthPortalKwargs(t, "anthropic/claude-opus-4.8", "https://api.anthropic.com")["model"]; got != "claude-opus-4-8" {
		t.Fatalf("native anthropic slug %v", got)
	}
}

// Request body the portal receives for a nous route carrying a session id
func anthPortalBody(t *testing.T, provider, baseURL, sessionID string) map[string]any {
	t.Helper()
	srv := newAnthServer(t)
	route := srv.route(baseURL, "portal-invoke-jwt")
	route.Provider = provider
	route.Model = "anthropic/claude-opus-4.8"
	req := anthUserRequest(route.Model, "hi")
	req.SessionID = sessionID
	if _, err := srv.client(route).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return srv.last().Body
}

func TestAnthropicPortalTagsAndSessionReachTheMessagesRequest(t *testing.T) {
	body := anthPortalBody(t, "nous", anthPortalURL, "sess-abc123")
	tags, _ := body["tags"].([]any)
	has := map[string]bool{}
	for _, tag := range tags {
		s, ok := tag.(string)
		if !ok {
			t.Fatalf("non string tag %v", tag)
		}
		has[s] = true
	}
	for _, want := range []string{"product=icebath-agent", "client=icebath-client-v" + baseVersion(), "conversation=sess-abc123"} {
		if !has[want] {
			t.Errorf("tag %q missing from %v", want, tags)
		}
	}
	if body["session_id"] != "sess-abc123" {
		t.Fatalf("session_id %v", body["session_id"])
	}
}

func TestAnthropicPortalMergeIsANoOpForNonNous(t *testing.T) {
	body := anthPortalBody(t, "anthropic", "https://api.anthropic.com", "s")
	if _, ok := body["tags"]; ok {
		t.Fatalf("tags sent to a non nous route: %v", body)
	}
	if _, ok := body["session_id"]; ok {
		t.Fatalf("session_id sent to a non nous route: %v", body)
	}
}

// One signed thinking turn before a tool result, as the agent records it
func anthPortalThinkingMessages() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "calling",
			"tool_calls": []any{map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"cmd":"ls"}`}}},
			"anthropic_content_blocks": []any{
				map[string]any{"type": "thinking", "thinking": "plan the listing", "signature": "sig-portal-abc"},
				map[string]any{"type": "text", "text": "calling"},
				map[string]any{"type": "tool_use", "id": "t1", "name": "terminal", "input": map[string]any{"cmd": "ls"}},
			}},
		{"role": "tool", "tool_call_id": "t1", "name": "terminal", "content": "done"},
	}
}

// Thinking and tool use blocks of the converted assistant turn
func anthPortalAssistantBlocks(t *testing.T, baseURL, model string) (thinking []map[string]any, toolUse bool) {
	t.Helper()
	_, converted := convertMessagesToAnthropic(anthPortalThinkingMessages(), convertOptions{baseURL: baseURL, model: model})
	for _, m := range converted {
		if m["role"] != "assistant" {
			continue
		}
		blocks, _ := m["content"].([]any)
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "thinking":
				thinking = append(thinking, block)
			case "tool_use":
				toolUse = true
			}
		}
		return thinking, toolUse
	}
	t.Fatal("no assistant turn")
	return nil, false
}

func TestAnthropicPortalKeepsSignedThinking(t *testing.T) {
	for _, tc := range []struct{ url, env string }{{anthPortalURL, ""}, {anthStagingURL, anthStagingURL}} {
		t.Run(tc.url, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("NOUS_INFERENCE_BASE_URL", tc.env)
			}
			thinking, toolUse := anthPortalAssistantBlocks(t, tc.url, "anthropic/claude-opus-4.8")
			if len(thinking) != 1 || thinking[0]["thinking"] != "plan the listing" || thinking[0]["signature"] != "sig-portal-abc" || !toolUse {
				t.Fatalf("thinking %v tool use %v", thinking, toolUse)
			}
		})
	}
}

func TestAnthropicOtherThirdPartyGatewaysStillStripThinking(t *testing.T) {
	if thinking, _ := anthPortalAssistantBlocks(t, "https://relay.example.com/anthropic", "claude-opus-4-8"); len(thinking) != 0 {
		t.Fatalf("thinking kept on a relay: %v", thinking)
	}
}
