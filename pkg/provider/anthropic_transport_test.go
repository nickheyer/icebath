package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func anthBareClient() *anthropicClient {
	return &anthropicClient{route: Route{Provider: "anthropic", Model: "claude-sonnet-4-6"}, transport: newAnthDirectTransport(Route{})}
}

func TestAnthropicTransportConvertToolsSimple(t *testing.T) {
	out := convertToolsToAnthropic([]map[string]any{{"type": "function", "function": map[string]any{
		"name": "test_tool", "description": "A test", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}})
	if len(out) != 1 || out[0]["name"] != "test_tool" {
		t.Fatalf("%v", out)
	}
	if _, ok := out[0]["input_schema"]; !ok {
		t.Fatalf("no input_schema %v", out[0])
	}
}

func TestAnthropicTransportMapFinishReason(t *testing.T) {
	for in, want := range map[string]string{"end_turn": "stop", "tool_use": "tool_calls", "max_tokens": "length", "stop_sequence": "stop",
		"refusal": "content_filter", "model_context_window_exceeded": "length", "unknown": "stop"} {
		if got := anthMapFinishReason(in); got != want {
			t.Fatalf("%s: %s want %s", in, got, want)
		}
	}
}

func TestAnthropicTransportNormalizeResponseText(t *testing.T) {
	msg := anthMessage("end_turn", anthText("Hello world"))
	msg["usage"], msg["model"] = map[string]any{"input_tokens": 10, "output_tokens": 5}, "claude-sonnet-4-6"
	resp, err := anthBareClient().normalizeResponse(msg, 200, nil, 0)
	if err != nil || resp.Message.Content != "Hello world" || len(resp.Message.ToolCalls) != 0 || resp.FinishReason != "stop" {
		t.Fatalf("%+v %v", resp, err)
	}
}

func TestAnthropicTransportRefusalSurfacesStopDetails(t *testing.T) {
	refusal := anthMessage("refusal")
	refusal["usage"], refusal["model"] = nil, "claude"
	refusal["stop_details"] = map[string]any{"type": "refusal", "category": "general_harms", "explanation": "classifier halt"}
	resp, err := anthBareClient().normalizeResponse(refusal, 200, nil, 0)
	if err != nil || resp.FinishReason != "content_filter" {
		t.Fatalf("%+v %v", resp, err)
	}
	details, _ := asMap(ResponseStopDetails(resp))
	if details["explanation"] != "classifier halt" {
		t.Fatalf("stop details %v", ResponseStopDetails(resp))
	}
	plain := anthMessage("end_turn", anthText("ok"))
	plain["usage"], plain["model"] = nil, "claude"
	resp, err = anthBareClient().normalizeResponse(plain, 200, nil, 0)
	if err != nil || ResponseStopDetails(resp) != nil {
		t.Fatalf("plain end_turn carried stop details %v %v", ResponseStopDetails(resp), err)
	}
}

func TestAnthropicTransportStreamedRefusalStopDetailsReachTheResponse(t *testing.T) {
	details := map[string]any{"type": "refusal", "category": "general_harms", "explanation": "classifier halt"}
	s := newAnthServer(t, anthReplySSE(
		map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "content": []any{},
			"model": "claude", "stop_reason": nil, "usage": map[string]any{"input_tokens": 3, "output_tokens": 0}}},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "refusal", "stop_details": details}, "usage": map[string]any{"output_tokens": 0}},
		map[string]any{"type": "message_stop"}))
	c := s.client(s.route("https://api.anthropic.com", "sk-ant-api03-key"))
	resp, err := c.Stream(t.Context(), anthUserRequest("claude-sonnet-4-20250514", "hi"), func(StreamEvent) error { return nil })
	if err != nil || resp.FinishReason != "content_filter" {
		t.Fatalf("%+v %v", resp, err)
	}
	anthMustEqualJSON(t, ResponseStopDetails(resp), details)
}

func TestAnthropicTransportNormalizeResponseToolCalls(t *testing.T) {
	msg := anthMessage("tool_use", anthToolUse("toolu_123", "terminal", map[string]any{"command": "ls"}))
	msg["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 20}
	resp, err := anthBareClient().normalizeResponse(msg, 200, nil, 0)
	if err != nil || resp.FinishReason != "tool_calls" || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("%+v %v", resp, err)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Function.Name != "terminal" || tc.ID != "toolu_123" || !strings.Contains(tc.Function.Arguments, `"command"`) {
		t.Fatalf("%+v", tc)
	}
}

func TestAnthropicTransportNativeBuildKeepsRequiredMaxTokens(t *testing.T) {
	kwargs, err := anthBuildKwargs(anthBuildInput{model: "claude-sonnet-4-5", messages: []map[string]any{{"role": "user", "content": "fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := anyInt(kwargs["max_tokens"]); n <= 0 {
		t.Fatalf("native Messages call needs a positive max_tokens %v", kwargs["max_tokens"])
	}
}

func TestAnthropicToPlainData(t *testing.T) {
	for _, v := range []any{"hello", 42, nil} {
		if got := deepCopyAny(v); got != v {
			t.Fatalf("plain value changed %v -> %v", v, got)
		}
	}
	got := deepCopyAny(json.RawMessage(`{"type":"thinking","thinking":"reason","signature":"sig"}`))
	anthMustEqualJSON(t, got, map[string]any{"type": "thinking", "thinking": "reason", "signature": "sig"})
}
