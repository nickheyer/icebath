package provider

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func bridgeTestTools() []any {
	return []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "memory", "description": "d1", "parameters": map[string]any{"a": 1}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "description": "d2", "parameters": map[string]any{}}},
	}
}

func bridgeSpecNames(specs []*pyjson.Object) []string {
	names := []string{}
	for _, s := range specs {
		n, _ := s.Get("name")
		names = append(names, n.(string))
	}
	return names
}

func bridgeCallIDs(calls []state.ToolCall) []string {
	ids := []string{}
	for _, c := range calls {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestSpecsAreFlattenedAndMalformedEntriesSkipped(t *testing.T) {
	specs := ToolSpecsFromOpenAITools(append(bridgeTestTools(), "junk", nil, map[string]any{"function": nil}, map[string]any{"function": map[string]any{"name": "  "}}), nil)
	if got := bridgeSpecNames(specs); !reflect.DeepEqual(got, []string{"memory", "read_file"}) {
		t.Fatalf("names %v", got)
	}
	want := pyjson.NewObject()
	want.Set("name", "memory")
	want.Set("description", "d1")
	want.Set("parameters", map[string]any{"a": 1})
	if !pyjson.Equal(specs[0], want) {
		t.Fatalf("spec %v", specs[0])
	}
	if !reflect.DeepEqual(specs[0].Keys, []string{"name", "description", "parameters"}) {
		t.Fatalf("spec key order %v", specs[0].Keys)
	}
}

func TestAllowlistForwardsOnlyTheNamedTools(t *testing.T) {
	if got := bridgeSpecNames(ToolSpecsFromOpenAITools(bridgeTestTools(), []string{"memory"})); !reflect.DeepEqual(got, []string{"memory"}) {
		t.Fatalf("names %v", got)
	}
	if n := len(ToolSpecsFromOpenAITools(bridgeTestTools(), nil)); n != 2 {
		t.Fatalf("no allowlist forwarded %d", n)
	}
	if specs := ToolSpecsFromOpenAITools(bridgeTestTools(), []string{}); len(specs) != 0 {
		t.Fatalf("empty allowlist forwarded %v", specs)
	}
}

func TestRenderedSectionsCarryTheContractAndTheSchemas(t *testing.T) {
	sections, err := RenderToolBridgeSections(bridgeTestTools(), map[string]any{"type": "function"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 2 {
		t.Fatalf("sections %v", sections)
	}
	if !strings.Contains(sections[0], "<tool_call>") {
		t.Fatalf("contract missing %q", sections[0])
	}
	_, body, _ := strings.Cut(sections[0], "\n")
	payload, err := pyjson.Loads(body)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, s := range payload.([]any) {
		n, _ := s.(*pyjson.Object).Get("name")
		names = append(names, n.(string))
	}
	if !reflect.DeepEqual(names, []string{"memory", "read_file"}) {
		t.Fatalf("payload names %v", names)
	}
	if !strings.HasPrefix(sections[1], "Tool choice hint:") {
		t.Fatalf("hint %q", sections[1])
	}
}

func TestNoToolsAndNoChoiceRenderNothing(t *testing.T) {
	for _, tools := range [][]any{nil, {}, {map[string]any{"function": map[string]any{}}}} {
		sections, err := RenderToolBridgeSections(tools, nil, nil)
		if err != nil || len(sections) != 0 {
			t.Fatalf("tools %v rendered %v %v", tools, sections, err)
		}
	}
}

func TestToolCallBlockIsParsedAndStrippedFromTheText(t *testing.T) {
	calls, cleaned, err := ExtractToolCallsFromText("Sure.\n<tool_call>{\"id\": \"c1\", \"type\": \"function\", \"function\": " +
		"{\"name\": \"memory\", \"arguments\": \"{\\\"action\\\": \\\"add\\\"}\"}}</tool_call>\nDone.")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Function.Name != "memory" || calls[0].ID != "c1" {
		t.Fatalf("calls %+v", calls)
	}
	args, err := pyjson.Loads(calls[0].Function.Arguments)
	if err != nil || !reflect.DeepEqual(pyjson.Plain(args), map[string]any{"action": "add"}) {
		t.Fatalf("arguments %q", calls[0].Function.Arguments)
	}
	if strings.Contains(cleaned, "<tool_call>") || cleaned != "Sure.\nDone." {
		t.Fatalf("cleaned %q", cleaned)
	}
}

func TestMultipleBlocksAreAllParsed(t *testing.T) {
	var b strings.Builder
	for _, i := range []string{"0", "1", "2"} {
		b.WriteString(`<tool_call>{"id": "c` + i + `", "type": "function", "function": {"name": "todo", "arguments": "{}"}}</tool_call>`)
	}
	calls, cleaned, err := ExtractToolCallsFromText(b.String())
	if err != nil {
		t.Fatal(err)
	}
	if got := bridgeCallIDs(calls); !reflect.DeepEqual(got, []string{"c0", "c1", "c2"}) || cleaned != "" {
		t.Fatalf("ids %v cleaned %q", got, cleaned)
	}
}

func TestNonStringArgumentsAreJsonEncodedAndMissingIdsSynthesised(t *testing.T) {
	calls, _, err := ExtractToolCallsFromText(`<tool_call>{"type": "function", "function": {"name": "todo", "arguments": {"op": "list"}}}</tool_call>`)
	if err != nil {
		t.Fatal(err)
	}
	if calls[0].Function.Arguments != `{"op": "list"}` || calls[0].ID != "acp_call_1" {
		t.Fatalf("call %+v", calls[0])
	}
	if calls[0].Extra["call_id"] != "acp_call_1" || calls[0].Type != "function" {
		t.Fatalf("call shape %+v", calls[0])
	}
}

func TestBareJsonIsAFallbackOnlyWhenNoBlockMatched(t *testing.T) {
	bare := `{"id": "c9", "type": "function", "function": {"name": "memory", "arguments": "{}"}}`
	calls, cleaned, err := ExtractToolCallsFromText("before " + bare + " after")
	if err != nil {
		t.Fatal(err)
	}
	if got := bridgeCallIDs(calls); !reflect.DeepEqual(got, []string{"c9"}) || cleaned != "before\nafter" {
		t.Fatalf("ids %v cleaned %q", got, cleaned)
	}
	calls, _, err = ExtractToolCallsFromText("<tool_call>" + bare + "</tool_call> and " + bare)
	if err != nil || len(calls) != 1 {
		t.Fatalf("double counted %v %v", calls, err)
	}
}

func TestMalformedAndEmptyInputNeverRaises(t *testing.T) {
	for _, text := range []string{"", "   "} {
		calls, cleaned, err := ExtractToolCallsFromText(text)
		if err != nil || len(calls) != 0 || cleaned != "" {
			t.Fatalf("%q gave %v %q %v", text, calls, cleaned, err)
		}
	}
	calls, cleaned, err := ExtractToolCallsFromText("<tool_call>{not json}</tool_call>plain")
	if err != nil || len(calls) != 0 || cleaned != "plain" {
		t.Fatalf("malformed gave %v %q %v", calls, cleaned, err)
	}
	calls, cleaned, err = ExtractToolCallsFromText(`<tool_call>{"function": 5}</tool_call>hi`)
	if err != nil || len(calls) != 0 || cleaned != "hi" {
		t.Fatalf("non call gave %v %q %v", calls, cleaned, err)
	}
}

func bridgeTestCompletion(extras map[string]any) map[string]any {
	message := map[string]any{
		"content":           "hello",
		"tool_calls":        []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "memory", "arguments": "{}"}}},
		"reasoning":         nil,
		"reasoning_content": nil,
	}
	completion := map[string]any{
		"choices": []any{map[string]any{"message": message, "finish_reason": "tool_calls"}},
		"usage":   map[string]any{"total_tokens": 3},
		"model":   "acp",
	}
	for k, v := range extras {
		completion[k] = v
	}
	return completion
}

func TestStreamChunksCarryTheDeltaThenTheUsage(t *testing.T) {
	chunks, err := CompletionToStreamChunks(bridgeTestCompletion(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks.Chunks) != 2 {
		t.Fatalf("chunks %v", chunks.Chunks)
	}
	choice := chunks.Chunks[0]["choices"].([]any)[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	if delta["content"] != "hello" {
		t.Fatalf("content %v", delta["content"])
	}
	call := delta["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["name"] != "memory" || call["index"] != 0 {
		t.Fatalf("tool call delta %v", call)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish %v", choice["finish_reason"])
	}
	if chunks.Chunks[0]["usage"] != nil || chunks.Chunks[1]["usage"].(map[string]any)["total_tokens"] != 3 {
		t.Fatalf("usage placement %v", chunks.Chunks)
	}
	if got := chunks.Chunks[1]["choices"].([]any); len(got) != 0 {
		t.Fatalf("usage chunk choices %v", got)
	}
	ctx := context.Background()
	for i := range 2 {
		if c, err := chunks.Next(ctx); err != nil || !reflect.DeepEqual(c, chunks.Chunks[i]) {
			t.Fatalf("chunk %d read %v %v", i, c, err)
		}
	}
	if _, err := chunks.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("after last chunk %v", err)
	}
}

func TestResponseLevelExtrasSurviveTheStreamConversion(t *testing.T) {
	projected := []any{map[string]any{"role": "tool", "content": "x"}}
	chunks, err := CompletionToStreamChunks(bridgeTestCompletion(map[string]any{"icebath_projected_messages": projected}))
	if err != nil {
		t.Fatal(err)
	}
	var stream AuxChatStream = chunks
	if stream == nil || len(chunks.Chunks) != 2 {
		t.Fatalf("not a chunk list %v", chunks)
	}
	if got := state.ToolCallField(chunks, "icebath_projected_messages"); !reflect.DeepEqual(got, projected) {
		t.Fatalf("projected %v", got)
	}
}

func TestATextOnlyCompletionStreamsWithoutToolCallDeltas(t *testing.T) {
	completion := bridgeTestCompletion(nil)
	choice := completion["choices"].([]any)[0].(map[string]any)
	choice["message"].(map[string]any)["tool_calls"] = []any{}
	choice["finish_reason"] = "stop"
	chunks, err := CompletionToStreamChunks(completion)
	if err != nil {
		t.Fatal(err)
	}
	delta := chunks.Chunks[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["tool_calls"] != nil {
		t.Fatalf("tool call deltas %v", delta["tool_calls"])
	}
}

func TestCopilotPromptStillCarriesTheContractAndTheTools(t *testing.T) {
	prompt, err := acpFormatMessagesAsPrompt([]any{map[string]any{"role": "user", "content": "hi"}}, bridgeTestTools(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<tool_call>{...}</tool_call>", `"name": "memory"`, `"name": "read_file"`, "hi"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q\n%s", want, prompt)
		}
	}
}
