package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Scripted chat completion answers served in order with every request body kept for assertions
type thinkingScript struct {
	mu        sync.Mutex
	responses []map[string]any
	bodies    []map[string]any
}

// Loopback provider replaying the script, GET answers the models list
func (s *thinkingScript) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "anthropic/claude-sonnet-4-20250514", "object": "model"}}})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		n := len(s.bodies)
		s.bodies = append(s.bodies, body)
		var resp map[string]any
		if n < len(s.responses) {
			resp = s.responses[n]
		}
		s.mu.Unlock()
		if resp == nil {
			t.Errorf("provider call %d past the %d scripted responses", n+1, len(s.responses))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Request bodies the provider saw so far
func (s *thinkingScript) calls() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

// finish_reason length with reasoning spent and zero visible content, the live GLM on ollama-cloud shape
func thinkingOnlyLengthResponse() map[string]any {
	return map[string]any{"id": "chatcmpl-thinking-exhausted", "model": "test/model", "usage": nil,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": ""}, "finish_reason": home.FinishReasonLength}}}
}

// Mock chat completion with the given content and finish reason like tests/agent/test_run_agent.py _mock_response
func thinkingMockResponse(content, finishReason string) map[string]any {
	return map[string]any{"model": "test/model", "usage": nil,
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": finishReason}}}
}

// Real OpenRouter shaped agent whose wire client talks to the scripted loopback provider
func thinkingLoopAgent(t *testing.T, script *thinkingScript) (*AIAgent, context.Context) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	srv := script.server(t)
	env := &tools.Env{Home: h, Cwd: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{Model: "anthropic/claude-sonnet-4-20250514", Provider: "openrouter", APIMode: "chat_completions", APIKey: "test-key-1234567890", BaseURL: srv.URL + "/v1",
		QuietMode: true, SkipMemory: true, SkipContextFiles: true, Cwd: h, Env: env, EnabledToolsets: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); provider.WaitReasoningCapsWarms() })
	a.setBaseURL("https://openrouter.ai/api/v1")
	a.DisableStreaming()
	a.cachedSystemPrompt = "You are helpful."
	a.usePromptCaching = false
	a.compressionEnabled = false
	a.saveTrajectories = false
	return a, ctx
}

// Runs one user turn through the conversation loop
func thinkingRun(t *testing.T, a *AIAgent, ctx context.Context, message string) map[string]any {
	t.Helper()
	result, err := a.RunConversation(ctx, message, RunConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Assistant rows of the transcript with neither visible content nor tool calls
func emptyAssistantRows(t *testing.T, messages any) []map[string]any {
	t.Helper()
	rows, _ := parityJSON(t, messages).([]any)
	var empty []map[string]any
	for _, row := range rows {
		m, _ := row.(map[string]any)
		if m["role"] != "assistant" {
			continue
		}
		content, _ := m["content"].(string)
		if strings.TrimSpace(content) == "" && m["tool_calls"] == nil {
			empty = append(empty, m)
		}
	}
	return empty
}

// Reasoning object a request body carried, nil when none went out
func wireReasoning(body map[string]any) map[string]any {
	reasoning, _ := body["reasoning"].(map[string]any)
	return reasoning
}

func TestThinkingOnlyTruncationRetryAfterThinkingOnlyTruncationCompletes(t *testing.T) {
	script := &thinkingScript{responses: []map[string]any{thinkingOnlyLengthResponse(), thinkingMockResponse("Here is the full answer.", "stop")}}
	a, ctx := thinkingLoopAgent(t, script)
	result := thinkingRun(t, a, ctx, "write me a long report")
	final, _ := result["final_response"].(string)
	if result["completed"] != true || !strings.Contains(final, "full answer") {
		t.Fatalf("result %v", result)
	}
	if empty := emptyAssistantRows(t, result["messages"]); len(empty) != 0 {
		t.Fatalf("An empty (thinking-only) truncated response must never be appended to the transcript: %v", empty)
	}
	calls := script.calls()
	if len(calls) != 2 {
		t.Fatalf("calls %d", len(calls))
	}
	if calls[1]["max_tokens"] != 8192.0 {
		t.Fatalf("The continuation retry must request a larger output budget than the request that truncated, got %v", calls[1]["max_tokens"])
	}
	if a.ephemeralReasoningOff {
		t.Fatal("The one-shot reasoning-off override must be consumed by the continuation call.")
	}
}

func TestThinkingOnlyTruncationFullCeilingWithEmptyFragmentsStillSettles(t *testing.T) {
	script := &thinkingScript{responses: []map[string]any{thinkingOnlyLengthResponse(), thinkingOnlyLengthResponse(), thinkingOnlyLengthResponse(), thinkingOnlyLengthResponse()}}
	a, ctx := thinkingLoopAgent(t, script)
	result := thinkingRun(t, a, ctx, "write me a long report")
	final, _ := result["final_response"].(string)
	if result["completed"] != false || result["partial"] != true || final == "" {
		t.Fatalf("An all-empty ceiling exit must still surface a user-facing message: %v", result)
	}
	if empty := emptyAssistantRows(t, result["messages"]); len(empty) != 0 {
		t.Fatalf("empty rows %v", empty)
	}
	if a.ephemeralReasoningOff {
		t.Fatal("The ceiling exit must clear the pending one-shot override so the next turn does not silently lose thinking.")
	}
}

func TestThinkingOnlyTruncationMixedFragmentsKeepVisibleText(t *testing.T) {
	script := &thinkingScript{responses: []map[string]any{thinkingMockResponse("visible part one. ", home.FinishReasonLength), thinkingOnlyLengthResponse(),
		thinkingMockResponse("and the ending.", "stop")}}
	a, ctx := thinkingLoopAgent(t, script)
	result := thinkingRun(t, a, ctx, "write me a long report")
	final, _ := result["final_response"].(string)
	if result["completed"] != true || !strings.Contains(final, "visible part one.") || !strings.Contains(final, "and the ending.") {
		t.Fatalf("result %v", result)
	}
	if empty := emptyAssistantRows(t, result["messages"]); len(empty) != 0 {
		t.Fatalf("empty rows %v", empty)
	}
}

func TestReasoningOffReachesTheWireContinuationRequestCarriesReasoningOffOnTheWire(t *testing.T) {
	script := &thinkingScript{responses: []map[string]any{thinkingOnlyLengthResponse(), thinkingMockResponse("Here is the full answer.", "stop")}}
	a, ctx := thinkingLoopAgent(t, script)
	a.reasoningConfig = map[string]any{"enabled": true, "effort": "high"}
	result := thinkingRun(t, a, ctx, "write me a long report")
	if result["completed"] != true {
		t.Fatalf("result %v", result)
	}
	calls := script.calls()
	if len(calls) != 2 {
		t.Fatalf("calls %d", len(calls))
	}
	paritySame(t, wireReasoning(calls[0]), map[string]any{"enabled": true, "effort": "high"}, "first request reasoning")
	second := wireReasoning(calls[1])
	if second == nil || second["enabled"] != false {
		t.Fatalf("continuation must be sent with thinking off, got %v", second)
	}
}

func TestReasoningOffReachesTheWireReasoningOffIsExactlyOneRequestAndPrefixStaysStable(t *testing.T) {
	script := &thinkingScript{responses: []map[string]any{thinkingOnlyLengthResponse(), thinkingMockResponse("PART ONE of the answer", home.FinishReasonLength),
		thinkingMockResponse(" and PART TWO, done.", "stop")}}
	a, ctx := thinkingLoopAgent(t, script)
	a.reasoningConfig = map[string]any{"enabled": true, "effort": "high"}
	result := thinkingRun(t, a, ctx, "write me a long report")
	final, _ := result["final_response"].(string)
	if result["completed"] != true || !strings.Contains(final, "PART ONE") || !strings.Contains(final, "PART TWO") {
		t.Fatalf("result %v", result)
	}
	calls := script.calls()
	if len(calls) != 3 {
		t.Fatalf("calls %d", len(calls))
	}
	paritySame(t, wireReasoning(calls[0]), map[string]any{"enabled": true, "effort": "high"}, "first request reasoning")
	paritySame(t, wireReasoning(calls[1]), map[string]any{"enabled": false, "effort": "none"}, "continuation reasoning")
	paritySame(t, wireReasoning(calls[2]), map[string]any{"enabled": true, "effort": "high"}, "reasoning must be restored on the very next request")
	prompts := map[string]bool{}
	for _, call := range calls {
		first := parityAt(t, call, "messages", 0)
		if parityAt(t, first, "role") == "system" {
			prompts[parityAt(t, first, "content").(string)] = true
		}
	}
	if len(prompts) != 1 {
		t.Fatalf("system prompt must be byte-identical across the retry sequence, got %d variants", len(prompts))
	}
	if a.ephemeralReasoningOff {
		t.Fatal("the override must be consumed")
	}
}

func TestReasoningOffReachesTheWireStaleFlagDoesNotLeakIntoNextTurn(t *testing.T) {
	script := &thinkingScript{responses: []map[string]any{thinkingMockResponse("fresh turn answer.", "stop")}}
	a, ctx := thinkingLoopAgent(t, script)
	a.reasoningConfig = map[string]any{"enabled": true, "effort": "high"}
	a.ephemeralReasoningOff = true
	result := thinkingRun(t, a, ctx, "hello")
	if result["completed"] != true {
		t.Fatalf("result %v", result)
	}
	calls := script.calls()
	if len(calls) == 0 {
		t.Fatal("no request reached the wire")
	}
	paritySame(t, wireReasoning(calls[0]), map[string]any{"enabled": true, "effort": "high"}, "first request reasoning")
}
