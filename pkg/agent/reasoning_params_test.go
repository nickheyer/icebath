package agent

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestReasoningReplayRouteIsCachedUntilARouteInputOrRejectionChanges(t *testing.T) {
	a := &AIAgent{apiMode: "chat_completions", provider: "deepseek", model: "deepseek-v4-pro", baseURL: "https://api.deepseek.com/v1"}
	a.baseURLLower = a.baseURL
	first := a.reasoningReplayRoute()
	want := provider.ReasoningReplayRouteFor("chat_completions", "deepseek", "deepseek-v4-pro", a.baseURL, false, map[string]bool{})
	if !reflect.DeepEqual(first, want) || !first.Pad {
		t.Fatalf("route %+v want %+v", first, want)
	}
	cached := a.reasoningRouteCache
	a.reasoningReplayRoute()
	if a.reasoningRouteCache != cached {
		t.Fatal("unchanged route recomputed")
	}
	a.reasoningRejectingRoutes[a.reasoningRouteKey()] = map[string]bool{"reasoning_content": true}
	rejected := a.reasoningReplayRoute()
	if a.reasoningRouteCache == cached || rejected.Carriers["reasoning_content"] {
		t.Fatalf("rejection not applied %+v", rejected)
	}
	a.provider, a.model, a.baseURL, a.baseURLLower = "mistral", "mistral-large", "https://api.mistral.ai/v1", "https://api.mistral.ai/v1"
	if r := a.reasoningReplayRoute(); r.Pad {
		t.Fatalf("switched route kept the must echo pad %+v", r)
	}
	beforeOptIn := a.reasoningRouteCache
	a.reasoningEchoFlag = true
	optIn := a.reasoningReplayRoute()
	if a.reasoningRouteCache == beforeOptIn || !a.reasoningRouteCache.echoOptIn ||
		!reflect.DeepEqual(optIn, provider.ReasoningReplayRouteFor("chat_completions", "mistral", "mistral-large", a.baseURL, true, map[string]bool{})) {
		t.Fatalf("reasoning_echo opt in not part of the route key %+v", optIn)
	}
	a.apiMode = "codex_responses"
	if r := a.reasoningReplayRoute(); r.Carriers != nil {
		t.Fatalf("non chat route kept carriers %+v", r)
	}
}

func TestCopyReasoningContentForAPIAppliesTheActiveRoutePolicy(t *testing.T) {
	a := &AIAgent{apiMode: "chat_completions", provider: "deepseek", model: "deepseek-v4-pro", baseURL: "https://api.deepseek.com/v1"}
	src := &state.Message{Role: "assistant", Content: "", Reasoning: "thought"}
	got := map[string]any{"role": "assistant", "content": ""}
	a.copyReasoningContentForAPI(src, got)
	want := map[string]any{"role": "assistant", "content": ""}
	route := a.reasoningReplayRoute()
	applyReasoningContentPolicy(state.MessageToDict(src), want, route.Pad, route.Carriers)
	if !reflect.DeepEqual(got, want) || got["reasoning_content"] == nil {
		t.Fatalf("copied %v want %v", got, want)
	}
}

func TestSanitizeToolCallsForStrictAPIStripsCodexFieldsAndKeepsGeminiSignatures(t *testing.T) {
	if (&AIAgent{apiMode: "codex_responses"}).shouldSanitizeToolCalls() || !(&AIAgent{apiMode: "chat_completions"}).shouldSanitizeToolCalls() {
		t.Fatal("sanitize gate is not every api mode but codex responses")
	}
	tc := func() map[string]any {
		return map[string]any{"id": "c1", "call_id": "c1", "response_item_id": "fc_1", "type": "function",
			"function": map[string]any{"name": "f", "arguments": "{}"}, "extra_content": map[string]any{"google": map[string]any{"thought_signature": "s"}}}
	}
	original := tc()
	msg := map[string]any{"role": "assistant", "tool_calls": []any{original, "opaque"}}
	sanitizeToolCallsForStrictAPI(msg, "mistral-large", "https://api.mistral.ai/v1", "mistral")
	calls := msg["tool_calls"].([]any)
	first := calls[0].(map[string]any)
	if first["call_id"] != nil || first["response_item_id"] != nil || first["extra_content"] != nil || first["id"] != "c1" || calls[1] != "opaque" {
		t.Fatalf("strict route calls %v", calls)
	}
	if original["call_id"] == nil {
		t.Fatal("internal history copy was mutated")
	}
	gem := map[string]any{"tool_calls": []any{tc()}}
	sanitizeToolCallsForStrictAPI(gem, "gemini-2.5-pro", "", "")
	kept := gem["tool_calls"].([]any)[0].(map[string]any)
	if kept["extra_content"] == nil || kept["call_id"] != nil {
		t.Fatalf("gemini route calls %v", kept)
	}
	untouched := map[string]any{"tool_calls": "x"}
	sanitizeToolCallsForStrictAPI(untouched, "", "", "")
	if untouched["tool_calls"] != "x" {
		t.Fatal("non list tool_calls rewritten")
	}
}
