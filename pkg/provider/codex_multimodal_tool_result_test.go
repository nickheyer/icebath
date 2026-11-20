package provider

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// History whose tool message carries OpenAI style content parts from the vision fast path
func multimodalToolResultHistory() []*state.Message {
	return []*state.Message{
		{Role: "user", Content: "What's in /tmp/foo.png?"},
		{Role: "assistant", Content: "", ToolCalls: []state.ToolCall{toolCall("call_abc", "vision_analyze", `{"image_url": "/tmp/foo.png", "question": "describe"}`)}},
		{Role: "tool", ToolName: "vision_analyze", ToolCallID: "call_abc", Content: jsonList([]map[string]any{
			{"type": "text", "text": "Image loaded."},
			{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,XYZ"}},
		})},
	}
}

// The single function_call_output item of the converted history
func functionCallOutput(t *testing.T, items []map[string]any) map[string]any {
	t.Helper()
	var outputs []map[string]any
	for _, item := range items {
		if item["type"] == "function_call_output" {
			outputs = append(outputs, item)
		}
	}
	if len(outputs) != 1 {
		t.Fatalf("function_call_output items %v", outputs)
	}
	return outputs[0]
}

func TestMultimodalToolResultConversionListContentBecomesOutputArray(t *testing.T) {
	out := functionCallOutput(t, convertHistory(t, multimodalToolResultHistory(), codexConvertOptions{}))
	if out["call_id"] != "call_abc" {
		t.Fatalf("call_id %v", out["call_id"])
	}
	parts, ok := out["output"].([]map[string]any)
	if !ok {
		t.Fatalf("Expected array output for multimodal tool result, got %T: %v", out["output"], out["output"])
	}
	types := map[any]bool{}
	for _, p := range parts {
		types[p["type"]] = true
	}
	if !types["input_text"] || !types["input_image"] {
		t.Fatalf("part types %v", types)
	}
}

func TestMultimodalToolResultConversionInputImagePreservesDataUrl(t *testing.T) {
	out := functionCallOutput(t, convertHistory(t, multimodalToolResultHistory(), codexConvertOptions{}))
	var images []map[string]any
	for _, p := range out["output"].([]map[string]any) {
		if p["type"] == "input_image" {
			images = append(images, p)
		}
	}
	if len(images) != 1 || images[0]["image_url"] != "data:image/png;base64,XYZ" {
		t.Fatalf("image parts %v", images)
	}
}

func TestMultimodalToolResultConversionStringToolContentStillStringOutput(t *testing.T) {
	msgs := []*state.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", ToolCalls: []state.ToolCall{toolCall("call_x", "terminal", "{}")}},
		{Role: "tool", ToolName: "terminal", ToolCallID: "call_x", Content: "ls output here"},
	}
	out := functionCallOutput(t, convertHistory(t, msgs, codexConvertOptions{}))
	if out["output"] != "ls output here" {
		t.Fatalf("output %#v", out["output"])
	}
}

func TestPreflightAcceptsArrayOutputPreflightPassesArrayThrough(t *testing.T) {
	raw := jsonList([]map[string]any{
		{"type": "function_call", "call_id": "call_abc", "name": "vision_analyze", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "call_abc", "output": []map[string]any{
			{"type": "input_text", "text": "Image loaded."},
			{"type": "input_image", "image_url": "data:image/png;base64,ABC"},
		}},
	})
	normalized, err := preflightCodexInputItems(raw, false, false)
	if err != nil {
		t.Fatal(err)
	}
	out := functionCallOutput(t, normalized)
	parts, ok := out["output"].([]map[string]any)
	if !ok || len(parts) != 2 || parts[1]["type"] != "input_image" || parts[1]["image_url"] != "data:image/png;base64,ABC" {
		t.Fatalf("output %#v", out["output"])
	}
}

func TestPreflightAcceptsArrayOutputPreflightDropsUnknownPartTypes(t *testing.T) {
	raw := jsonList([]map[string]any{
		{"type": "function_call", "call_id": "call_abc", "name": "vision_analyze", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "call_abc", "output": []map[string]any{
			{"type": "input_text", "text": "ok"},
			{"type": "garbage", "data": "nope"},
			{"type": "input_image", "image_url": "data:image/png;base64,ZZ"},
		}},
	})
	normalized, err := preflightCodexInputItems(raw, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, p := range functionCallOutput(t, normalized)["output"].([]map[string]any) {
		types = append(types, p["type"].(string))
	}
	if strings.Join(types, ",") != "input_text,input_image" {
		t.Fatalf("types %v", types)
	}
}

func TestCoerceToolNameValidPassthrough(t *testing.T) {
	for _, name := range []string{"web_search", "exec-command", "a1_B2-c3", strings.Repeat("x", 64)} {
		if CoerceToolName(name, "invalid_tool_call") != name {
			t.Fatalf("%q changed", name)
		}
	}
}

func TestCoerceToolNameCoercesInvalidChars(t *testing.T) {
	for name, want := range map[string]string{"exec.command": "exec_command", "run shell cmd": "run_shell_cmd", "weird..__name": "weird_name", "  tool!  ": "tool"} {
		if got := CoerceToolName(name, "invalid_tool_call"); got != want {
			t.Fatalf("%q -> %q want %q", name, got, want)
		}
	}
}

func TestCoerceToolNameDegenerateInputs(t *testing.T) {
	for _, name := range []any{"", "...", "日本語", nil} {
		if got := CoerceToolName(name, "fn"); got != "fn" {
			t.Fatalf("%v -> %q", name, got)
		}
	}
	if got := CoerceToolName(strings.Repeat("a.", 100), "invalid_tool_call"); len(got) > 64 {
		t.Fatalf("too long %q", got)
	}
}

func TestPreflightCodexInputItemsDropsShortIdForGithubResponses(t *testing.T) {
	items, err := preflightCodexInputItems(jsonList([]map[string]any{{
		"type": "message", "role": "assistant", "status": "in_progress",
		"content": []map[string]any{{"type": "output_text", "text": "pong"}},
		"id":      "msg_abc123", "phase": "final_answer",
	}}), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := items[0]["id"]; has {
		t.Fatalf("id kept: %v", items[0])
	}
	if items[0]["status"] != "in_progress" || items[0]["phase"] != "final_answer" {
		t.Fatalf("item %v", items[0])
	}
	sameJSON(t, items[0]["content"], []map[string]any{{"type": "output_text", "text": "pong"}}, "content")
}

func TestChatContentToResponsesPartsNonListReturnsEmpty(t *testing.T) {
	for _, content := range []any{"hi", nil} {
		parts, err := chatContentToResponsesParts(content, "user")
		if err != nil || len(parts) != 0 {
			t.Fatalf("%v -> %v %v", content, parts, err)
		}
	}
}

func TestChatContentToResponsesPartsTextPartsBecomeInputText(t *testing.T) {
	parts, err := chatContentToResponsesParts(jsonList([]map[string]any{{"type": "text", "text": "hello"}}), "user")
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, parts, []map[string]any{{"type": "input_text", "text": "hello"}}, "parts")
}
