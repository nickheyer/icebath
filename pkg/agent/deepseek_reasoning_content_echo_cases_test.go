package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Bare agent on the route the case names, the object.__new__ shape the hermes cases build
func echoAgent(providerName, model, baseURL string) *AIAgent {
	a := &AIAgent{provider: providerName, model: model}
	a.setBaseURL(baseURL)
	return a
}

func sdkToolCall() state.ToolCall {
	return state.ToolCall{ID: "c1", Type: "function", Function: state.ToolCallFunction{Name: "terminal", Arguments: "{}"}, Extra: map[string]any{"call_id": "c1"}}
}

func TestNeedsDeepSeekToolReasoning(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_provider_deepseek", func(t *testing.T) {
		if !echoAgent("deepseek", "deepseek-v4-flash", "").needsDeepseekToolReasoning() {
			t.Fatal("deepseek provider not recognised")
		}
	})
}

func TestCopyReasoningContentForApi(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_deepseek_tool_call_poisoned_history_gets_space_placeholder", func(t *testing.T) {
		a := echoAgent("deepseek", "deepseek-v4-flash", "")
		src := state.MessageFromDict(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "terminal"}}}})
		apiMsg := map[string]any{}
		a.copyReasoningContentForAPI(src, apiMsg)
		if apiMsg["reasoning_content"] != " " {
			t.Fatalf("api msg %v", apiMsg)
		}
	})
	t.Run("test_non_thinking_provider_strips_empty_reasoning_content", func(t *testing.T) {
		a := echoAgent("openrouter", "anthropic/claude-sonnet-4.6", "https://openrouter.ai/api/v1")
		src := state.MessageFromDict(map[string]any{"role": "assistant", "content": "hi", "reasoning_content": ""})
		apiMsg := map[string]any{}
		a.copyReasoningContentForAPI(src, apiMsg)
		if _, ok := apiMsg["reasoning_content"]; ok {
			t.Fatalf("api msg %v", apiMsg)
		}
	})
}

func TestBuildAssistantMessageDeepSeekReasoningContent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_deepseek_tool_call_reasoning_is_backfilled_into_reasoning_content", func(t *testing.T) {
		a := echoAgent("deepseek", "deepseek-v4-flash", "")
		resp := &provider.Response{Message: &state.Message{Role: "assistant", Reasoning: "DeepSeek tool-call reasoning",
			ToolCalls: []state.ToolCall{{ID: "call_1", Type: "function", Function: state.ToolCallFunction{Name: "terminal", Arguments: "{}"}}}}}
		msg := a.buildAssistantMessage(resp, "tool_calls")
		if msg["reasoning_content"] != "DeepSeek tool-call reasoning" || state.ToolCallField(msg["tool_calls"].([]any)[0], "id") != "call_1" {
			t.Fatalf("msg %v", msg)
		}
	})
}

func TestBuildAssistantMessagePadsStrictProviders(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_tool_call_reasoning_content_pad", func(t *testing.T) {
		for _, c := range []struct {
			id, provider, model, baseURL string
			noneAttr                     bool
			expected                     any
		}{
			{"deepseek-attr-none", "deepseek", "deepseek-v4-pro", "", true, " "},
			{"deepseek-attr-absent", "deepseek", "deepseek-v4-pro", "", false, " "},
			{"kimi-attr-none", "kimi-coding", "kimi-k2.6", "", true, " "},
			{"moonshot-base-url", "custom", "kimi-k2", "https://api.moonshot.ai/v1", false, " "},
			{"openrouter-no-pad", "openrouter", "anthropic/claude-sonnet-4.6", "https://openrouter.ai/api/v1", false, nil},
		} {
			a := echoAgent(c.provider, c.model, c.baseURL)
			m := &state.Message{Role: "assistant", ToolCalls: []state.ToolCall{sdkToolCall()}}
			if c.noneAttr {
				m.Extra = map[string]any{"reasoning_content": nil}
			}
			msg := a.buildAssistantMessage(&provider.Response{Message: m}, "tool_calls")
			got, present := msg["reasoning_content"]
			if c.expected == nil {
				if present {
					t.Fatalf("%s padded %v", c.id, msg)
				}
				continue
			}
			if got != c.expected {
				t.Fatalf("%s reasoning_content %v", c.id, got)
			}
		}
	})
	t.Run("test_tool_call_preserves_real_reasoning_content", func(t *testing.T) {
		a := echoAgent("deepseek", "deepseek-v4-pro", "")
		msg := a.buildAssistantMessage(&provider.Response{Message: &state.Message{Role: "assistant", ReasoningContent: "actual chain of thought", ToolCalls: []state.ToolCall{sdkToolCall()}}}, "tool_calls")
		if msg["reasoning_content"] != "actual chain of thought" {
			t.Fatalf("msg %v", msg)
		}
	})
	t.Run("test_text_only_turn_not_padded_by_tool_call_branch", func(t *testing.T) {
		a := echoAgent("deepseek", "deepseek-v4-pro", "")
		msg := a.buildAssistantMessage(&provider.Response{Message: &state.Message{Role: "assistant", Content: "hello"}}, "stop")
		_, calls := msg["tool_calls"]
		_, rc := msg["reasoning_content"]
		if calls || rc {
			t.Fatalf("msg %v", msg)
		}
	})
}

func TestNeedsKimiToolReasoning(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_kimi_signals", func(t *testing.T) {
		for _, c := range [][2]string{{"kimi-coding", ""}, {"kimi-coding-cn", ""}, {"custom", "https://api.kimi.com/v1"}, {"custom", "https://api.moonshot.ai/v1"}, {"custom", "https://api.moonshot.cn/v1"}} {
			if !echoAgent(c[0], "kimi-k2", c[1]).needsKimiToolReasoning() {
				t.Fatalf("%v not kimi", c)
			}
		}
	})
}

func codexBuiltHistory() []map[string]any {
	return []map[string]any{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "do the thing"},
		{"role": "assistant", "content": "", "reasoning_content": "summary from codex", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "terminal"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "ok"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c2", "function": map[string]any{"name": "terminal"}}}},
		{"role": "tool", "tool_call_id": "c2", "content": "ok"},
	}
}

func TestReapplyReasoningEchoForProviderSwitch(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_switch_to_deepseek_pads_bare_turns", func(t *testing.T) {
		a := echoAgent("deepseek", "deepseek-v4-pro", "")
		msgs := codexBuiltHistory()
		if padded := a.reapplyReasoningEchoForProvider(msgs); padded != 1 {
			t.Fatalf("padded %d", padded)
		}
		for _, m := range msgs {
			if m["role"] == "assistant" && !pyTruthy(m["reasoning_content"]) {
				t.Fatalf("bare turn left %v", m)
			}
		}
		if msgs[2]["reasoning_content"] != "summary from codex" || msgs[4]["reasoning_content"] != " " {
			t.Fatalf("msgs %v", msgs)
		}
	})
	t.Run("test_strips_stale_pad_under_strict_provider", func(t *testing.T) {
		a := echoAgent("openai-codex", "gpt-5.5", "https://chatgpt.com/backend-api/codex")
		a.apiMode = "codex_responses"
		msgs := codexBuiltHistory()
		if changed := a.reapplyReasoningEchoForProvider(msgs); changed != 1 {
			t.Fatalf("changed %d", changed)
		}
		_, two := msgs[2]["reasoning_content"]
		_, four := msgs[4]["reasoning_content"]
		if two || four {
			t.Fatalf("msgs %v", msgs)
		}
	})
}

func deepseekBuiltHistory() []map[string]any {
	call := func(id string) []any {
		return []any{map[string]any{"id": id, "function": map[string]any{"name": "terminal"}}}
	}
	return []map[string]any{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "u1"},
		{"role": "assistant", "reasoning_content": " ", "tool_calls": call("a")},
		{"role": "tool", "tool_call_id": "a", "content": "ok"},
		{"role": "assistant", "content": "done"},
		{"role": "user", "content": "u2"},
		{"role": "assistant", "reasoning_content": " ", "tool_calls": call("b")},
		{"role": "tool", "tool_call_id": "b", "content": "ok"},
	}
}

func TestReasoningPrimaryToStrictFallback(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_mistral_fallback_strips_space_pad", func(t *testing.T) {
		msgs := deepseekBuiltHistory()
		if changed := echoAgent("mistral", "mistral-small-latest", "https://api.mistral.ai/v1").reapplyReasoningEchoForProvider(msgs); changed != 2 {
			t.Fatalf("changed %d", changed)
		}
		for i, m := range msgs {
			if _, ok := m["reasoning_content"]; ok {
				t.Fatalf("leak at %d %v", i, m)
			}
		}
	})
	t.Run("test_roundtrip_back_to_deepseek_repads", func(t *testing.T) {
		msgs := deepseekBuiltHistory()
		echoAgent("mistral", "mistral-small-latest", "https://api.mistral.ai/v1").reapplyReasoningEchoForProvider(msgs)
		echoAgent("deepseek", "deepseek-v4-pro", "").reapplyReasoningEchoForProvider(msgs)
		if msgs[2]["reasoning_content"] != " " || msgs[6]["reasoning_content"] != " " {
			t.Fatalf("msgs %v", msgs)
		}
	})
	t.Run("test_copy_strips_space_pad_for_mistral", func(t *testing.T) {
		src := state.MessageFromDict(map[string]any{"role": "assistant", "reasoning_content": " ", "tool_calls": []any{map[string]any{"id": "a"}}})
		apiMsg := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "a"}}}
		echoAgent("mistral", "mistral-small-latest", "https://api.mistral.ai/v1").copyReasoningContentForAPI(src, apiMsg)
		if _, ok := apiMsg["reasoning_content"]; ok {
			t.Fatalf("api msg %v", apiMsg)
		}
	})
}
