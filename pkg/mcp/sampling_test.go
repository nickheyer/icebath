package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeChatCall struct {
	messages, toolDefs []map[string]any
	opts               tools.AuxOptions
}

type fakeChatAux struct {
	mu    sync.Mutex
	resp  map[string]any
	delay time.Duration
	calls []fakeChatCall
}

func (f *fakeChatAux) Complete(context.Context, []*state.Message, tools.AuxOptions) (string, error) {
	return "", nil
}

func (f *fakeChatAux) CompleteChat(_ context.Context, messages, toolDefs []map[string]any, opts tools.AuxOptions) (map[string]any, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeChatCall{messages, toolDefs, opts})
	f.mu.Unlock()
	time.Sleep(f.delay)
	return f.resp, nil
}

func llmResponse(content any, finish string, toolCalls []any) map[string]any {
	return map[string]any{"model": "test-model", "usage": map[string]any{"total_tokens": 42},
		"choices": []any{map[string]any{"finish_reason": finish, "message": map[string]any{"content": content, "tool_calls": toolCalls}}}}
}

func llmToolResponse(calls ...[3]string) map[string]any {
	if len(calls) == 0 {
		calls = [][3]string{{"call_1", "get_weather", `{"city": "London"}`}}
	}
	list := make([]any, len(calls))
	for i, c := range calls {
		list[i] = map[string]any{"id": c[0], "function": map[string]any{"name": c[1], "arguments": c[2]}}
	}
	return llmResponse(nil, "tool_calls", list)
}

func samplingParams(messages ...SamplingMessage) *CreateMessageParams {
	if len(messages) == 0 {
		messages = []SamplingMessage{{Role: "user", Content: json.RawMessage(`{"text":"Hello"}`)}}
	}
	return &CreateMessageParams{Messages: messages, MaxTokens: 100}
}

func newTestSampler(t *testing.T, cfg map[string]any, resp map[string]any) (*samplingHandler, *fakeChatAux) {
	t.Helper()
	aux := &fakeChatAux{resp: resp}
	return newSamplingHandler("srv", &tools.Env{Aux: aux}, cfg).(*samplingHandler), aux
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSamplingHandlerInitCustomConfig(t *testing.T) {
	h, _ := newTestSampler(t, map[string]any{"max_rpm": 20, "timeout": 60, "max_tokens_cap": 2048, "max_tool_rounds": 3,
		"model": "gpt-4o", "allowed_models": []any{"gpt-4o", "gpt-3.5-turbo"}, "log_level": "debug"}, nil)
	if h.maxRPM != 20 || h.timeout != 60.0 || h.maxTokensCap != 2048 || h.maxToolRounds != 3 || h.modelOverride != "gpt-4o" ||
		h.auditLevel != slog.LevelDebug || !reflect.DeepEqual(h.allowedModels, []any{"gpt-4o", "gpt-3.5-turbo"}) {
		t.Fatalf("handler = %+v", h)
	}
}

func TestRateLimitRejectsOverLimit(t *testing.T) {
	h, _ := newTestSampler(t, map[string]any{"max_rpm": 3}, nil)
	for range 3 {
		if !h.checkRateLimit() {
			t.Fatal("request under the limit refused")
		}
	}
	if h.checkRateLimit() {
		t.Fatal("fourth request admitted")
	}
}

func TestRateLimitWindowExpiry(t *testing.T) {
	h, _ := newTestSampler(t, map[string]any{"max_rpm": 3}, nil)
	old := samplingNow() - 61
	h.rateTimestamps = []float64{old, old, old}
	if !h.checkRateLimit() {
		t.Fatal("expired timestamps were not purged")
	}
}

func TestSamplingResolveModel(t *testing.T) {
	prefs := &ModelPreferences{Hints: []ModelHint{{Name: "hint-model"}}}
	t.Run("test_config_override_wins", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{}, nil)
		h.modelOverride = "override-model"
		if got := h.resolveModel(prefs); got != "override-model" {
			t.Fatalf("override = %q", got)
		}
	})
	t.Run("test_hint_used_when_no_override", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{}, nil)
		if got := h.resolveModel(prefs); got != "hint-model" {
			t.Fatalf("hint = %q", got)
		}
	})
}

func TestSamplingConvertMessages(t *testing.T) {
	t.Run("test_single_text_message", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{}, nil)
		msgs, _, _ := h.buildLLMCall(samplingParams(SamplingMessage{Role: "user", Content: json.RawMessage(`{"text":"Hello world"}`)}), "")
		if !reflect.DeepEqual(msgs, []map[string]any{{"role": "user", "content": "Hello world"}}) {
			t.Fatalf("text = %#v", msgs)
		}
	})
	t.Run("test_tool_use_message", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{}, nil)
		msgs, _, _ := h.buildLLMCall(samplingParams(SamplingMessage{Role: "assistant",
			Content: json.RawMessage(`[{"id":"call_2","name":"get_weather","input":{"city":"London"}}]`)}), "")
		if len(msgs) != 1 || msgs[0]["role"] != "assistant" {
			t.Fatalf("tool use = %#v", msgs)
		}
		calls := msgs[0]["tool_calls"].([]any)
		fn := calls[0].(map[string]any)["function"].(map[string]any)
		var args map[string]any
		if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil || len(calls) != 1 || fn["name"] != "get_weather" ||
			!reflect.DeepEqual(args, map[string]any{"city": "London"}) {
			t.Fatalf("tool call = %#v %v", calls, err)
		}
	})
}

func TestSamplingCallbackText(t *testing.T) {
	t.Run("test_text_response", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{}, llmResponse("Hello from LLM", "stop", nil))
		res, rpcErr := h.CreateMessage(context.Background(), samplingParams())
		want := `{"content":{"text":"Hello from LLM","type":"text"},"model":"test-model","role":"assistant","stopReason":"endTurn"}`
		if rpcErr != nil || mustJSON(t, res) != want {
			t.Fatalf("result = %s %v", mustJSON(t, res), rpcErr)
		}
	})
	t.Run("test_server_tools_with_object_schema_are_normalized", func(t *testing.T) {
		h, aux := newTestSampler(t, map[string]any{}, llmResponse("LLM response", "stop", nil))
		params := samplingParams()
		params.Tools = []SamplingTool{{Name: "ask", Description: "Ask Crawl4AI", InputSchema: map[string]any{"type": "object"}}}
		if _, rpcErr := h.CreateMessage(context.Background(), params); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		want := `[{"function":{"description":"Ask Crawl4AI","name":"ask","parameters":{"properties":{},"required":[],"type":"object"}},"type":"function"}]`
		if got := mustJSON(t, aux.calls[0].toolDefs); got != want {
			t.Fatalf("tools = %s", got)
		}
	})
}

func TestSamplingCallbackToolUseToolUseResponse(t *testing.T) {
	h, _ := newTestSampler(t, map[string]any{}, llmToolResponse())
	res, rpcErr := h.CreateMessage(context.Background(), samplingParams())
	want := `{"content":[{"id":"call_1","input":{"city":"London"},"name":"get_weather","type":"tool_use"}],"model":"test-model","role":"assistant","stopReason":"toolUse"}`
	if rpcErr != nil || mustJSON(t, res) != want {
		t.Fatalf("result = %s %v", mustJSON(t, res), rpcErr)
	}
}

func TestSamplingToolLoopGovernance(t *testing.T) {
	t.Run("test_max_tool_rounds_enforcement", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{"max_tool_rounds": 2}, llmToolResponse())
		for i := range 2 {
			if _, rpcErr := h.CreateMessage(context.Background(), samplingParams()); rpcErr != nil {
				t.Fatalf("round %d refused: %v", i+1, rpcErr)
			}
		}
		if res, rpcErr := h.CreateMessage(context.Background(), samplingParams()); rpcErr == nil {
			t.Fatalf("third round admitted: %v", res)
		}
	})
	t.Run("test_max_tool_rounds_zero_disables", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{"max_tool_rounds": 0}, llmToolResponse())
		if res, rpcErr := h.CreateMessage(context.Background(), samplingParams()); rpcErr == nil {
			t.Fatalf("disabled tool loop admitted: %v", res)
		}
	})
}

func TestSamplingErrors(t *testing.T) {
	t.Run("test_rate_limit_error", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{"max_rpm": 1}, llmResponse("LLM response", "stop", nil))
		if _, rpcErr := h.CreateMessage(context.Background(), samplingParams()); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		_, rpcErr := h.CreateMessage(context.Background(), samplingParams())
		if rpcErr == nil || !strings.Contains(strings.ToLower(rpcErr.Message), "rate limit") || h.Metrics()["errors"] != 1 {
			t.Fatalf("rate limit = %v %v", rpcErr, h.Metrics())
		}
	})
	t.Run("test_timeout_error", func(t *testing.T) {
		h, aux := newTestSampler(t, map[string]any{}, llmResponse("LLM response", "stop", nil))
		h.timeout, aux.delay = 0.05, 150*time.Millisecond
		_, rpcErr := h.CreateMessage(context.Background(), samplingParams())
		if rpcErr == nil || !strings.Contains(strings.ToLower(rpcErr.Message), "timed out") || h.Metrics()["errors"] != 1 {
			t.Fatalf("timeout = %v %v", rpcErr, h.Metrics())
		}
	})
	t.Run("test_empty_choices_returns_error", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{}, map[string]any{"choices": []any{}, "model": "test-model", "usage": map[string]any{"total_tokens": 0}})
		_, rpcErr := h.CreateMessage(context.Background(), samplingParams())
		if rpcErr == nil || !strings.Contains(strings.ToLower(rpcErr.Message), "empty response") || h.Metrics()["errors"] != 1 {
			t.Fatalf("empty choices = %v %v", rpcErr, h.Metrics())
		}
	})
}

func TestSamplingModelWhitelist(t *testing.T) {
	t.Run("test_allowed_model_passes", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{"allowed_models": []any{"gpt-4o", "test-model"}}, llmResponse("LLM response", "stop", nil))
		if _, rpcErr := h.CreateMessage(context.Background(), samplingParams()); rpcErr != nil {
			t.Fatal(rpcErr)
		}
	})
	t.Run("test_disallowed_model_rejected", func(t *testing.T) {
		h, aux := newTestSampler(t, map[string]any{"allowed_models": []any{"gpt-4o"}, "model": "test-model"}, llmResponse("LLM response", "stop", nil))
		_, rpcErr := h.CreateMessage(context.Background(), samplingParams())
		if rpcErr == nil || !strings.Contains(rpcErr.Message, "not allowed") || h.Metrics()["errors"] != 1 || len(aux.calls) != 0 {
			t.Fatalf("disallowed = %v %v", rpcErr, h.Metrics())
		}
	})
}

func TestSamplingMalformedToolCallArgs(t *testing.T) {
	t.Run("test_invalid_json_wrapped_as_raw", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{}, llmToolResponse([3]string{"call_x", "some_tool", "not valid json {{{"}))
		res, rpcErr := h.CreateMessage(context.Background(), samplingParams())
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		tc := res.(map[string]any)["content"].([]any)[0].(map[string]any)
		if tc["type"] != "tool_use" || !reflect.DeepEqual(tc["input"], map[string]any{"_raw": "not valid json {{{"}) {
			t.Fatalf("tool use = %#v", tc)
		}
	})
}

// Capabilities a session advertises for a sampler built from cfg
func samplerCapabilities(t *testing.T, cfg map[string]any) (*samplingHandler, string) {
	t.Helper()
	h, _ := newTestSampler(t, cfg, nil)
	return h, mustJSON(t, (&Session{opts: SessionOptions{Sampler: h}}).capabilities())
}

func TestSamplingMetricsAndCapabilities(t *testing.T) {
	t.Run("test_request_and_token_metrics", func(t *testing.T) {
		h, _ := newTestSampler(t, map[string]any{}, llmResponse("LLM response", "stop", nil))
		if _, rpcErr := h.CreateMessage(context.Background(), samplingParams()); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		if m := h.Metrics(); m["requests"] != 1 || m["tokens_used"] != 42 || m["errors"] != 0 {
			t.Fatalf("metrics = %v", m)
		}
	})
	t.Run("test_default_omits_tools_capability", func(t *testing.T) {
		h, caps := samplerCapabilities(t, map[string]any{})
		if h.exposeClientTools || caps != `{"sampling":{}}` {
			t.Fatalf("exposed %v capabilities %s", h.exposeClientTools, caps)
		}
	})
	t.Run("test_sampling_tools_capability_is_opt_in", func(t *testing.T) {
		h, caps := samplerCapabilities(t, map[string]any{"expose_client_tools": true})
		if !h.exposeClientTools || caps != `{"sampling":{"tools":{}}}` {
			t.Fatalf("exposed %v capabilities %s", h.exposeClientTools, caps)
		}
	})
	t.Run("test_sampling_tools_capability_parses_bool_flags", func(t *testing.T) {
		for _, c := range []struct {
			raw  any
			want string
		}{{false, `{"sampling":{}}`}, {"false", `{"sampling":{}}`}, {"0", `{"sampling":{}}`}, {"off", `{"sampling":{}}`},
			{true, `{"sampling":{"tools":{}}}`}, {"true", `{"sampling":{"tools":{}}}`}, {"1", `{"sampling":{"tools":{}}}`}, {"yes", `{"sampling":{"tools":{}}}`}} {
			if _, caps := samplerCapabilities(t, map[string]any{"expose_client_tools": c.raw}); caps != c.want {
				t.Fatalf("expose_client_tools=%v capabilities = %s", c.raw, caps)
			}
		}
	})
}
