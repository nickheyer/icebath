package provider

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Tool call entry as a transport normalizes it, provider fields kept as extra keys
func normalizedToolCall(name, arguments string, providerFields map[string]any) state.ToolCall {
	return state.ToolCall{ID: "1", Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: arguments}, Extra: providerFields}
}

func TestToolCallBackwardCompatFunctionNameMatches(t *testing.T) {
	tc := normalizedToolCall("search", `{"q":"test"}`, nil)
	if tc.Function.Name != "search" || state.ToolCallField(tc.Function, "name") != "search" {
		t.Fatalf("%+v", tc)
	}
}

func TestToolCallBackwardCompatFunctionArgumentsMatches(t *testing.T) {
	tc := normalizedToolCall("search", `{"q":"test"}`, nil)
	if tc.Function.Arguments != `{"q":"test"}` || state.ToolCallField(tc.Function, "arguments") != tc.Function.Arguments {
		t.Fatalf("%+v", tc)
	}
}

func TestToolCallBackwardCompatGetattrPatternMatchesAgentLoop(t *testing.T) {
	tc := normalizedToolCall("fn", "{}", map[string]any{"call_id": "c1"})
	if got := state.ToolCallField(tc, "call_id"); got != "c1" {
		t.Fatalf("call_id %v", got)
	}
	if got := state.ToolCallField(normalizedToolCall("fn", "{}", nil), "call_id"); got != nil {
		t.Fatalf("call_id without provider data %v", got)
	}
}

func TestToolCallBackwardCompatExtraContentGetattrPattern(t *testing.T) {
	ec := map[string]any{"google": map[string]any{"thought_signature": "SIG_ABC123"}}
	tc := normalizedToolCall("fn", "{}", map[string]any{"extra_content": ec})
	if got := state.ToolCallField(tc, "extra_content"); !reflect.DeepEqual(got, ec) {
		t.Fatalf("extra_content %v", got)
	}
	if got := state.ToolCallField(normalizedToolCall("fn", "{}", nil), "extra_content"); got != nil {
		t.Fatalf("extra_content without provider data %v", got)
	}
}

func TestNormalizedResponseBackwardCompatReasoningContentFromProviderData(t *testing.T) {
	nr := &Response{Message: &state.Message{Role: "assistant", Content: "hi", ReasoningContent: "thought process"}, FinishReason: "stop"}
	if got := state.ToolCallField(nr.Message, "reasoning_content"); got != "thought process" {
		t.Fatalf("%v", got)
	}
}

func TestNormalizedResponseBackwardCompatReasoningContentNoneWhenAbsent(t *testing.T) {
	nr := &Response{Message: &state.Message{Role: "assistant", Content: "hi"}, FinishReason: "stop"}
	if got := state.ToolCallField(nr.Message, "reasoning_content"); got != "" {
		t.Fatalf("%v", got)
	}
}

func TestNormalizedResponseBackwardCompatReasoningDetailsFromProviderData(t *testing.T) {
	details := []any{map[string]any{"type": "thinking", "thinking": "hmm"}}
	raw, _ := json.Marshal(details)
	nr := &Response{Message: &state.Message{Role: "assistant", Content: "hi", ReasoningDetails: raw}, FinishReason: "stop"}
	var got []any
	if err := json.Unmarshal(nr.Message.ReasoningDetails, &got); err != nil || !reflect.DeepEqual(got, details) {
		t.Fatalf("%v %v", got, err)
	}
}
