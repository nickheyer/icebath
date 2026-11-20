package agent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func toolTail() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "edit the file"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "patch", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "ok edited"},
	}
}

func assertNoToolThenUser(t *testing.T, messages []map[string]any) {
	t.Helper()
	for i := 0; i < len(messages)-1; i++ {
		if messages[i]["role"] == "tool" && messages[i+1]["role"] == "user" {
			t.Fatalf("role-alternation violation: tool → user at index %d", i)
		}
	}
}

func TestClosingMakesNextUserMessageAlternationSafe(t *testing.T) {
	messages := toolTail()
	closeInterruptedToolSequence(&messages, nil, false)
	if _, ok := messages[len(messages)-1]["timestamp"].(float64); !ok {
		t.Fatalf("timestamp not a float: %#v", messages[len(messages)-1]["timestamp"])
	}
	assertNoToolThenUser(t, append(append([]map[string]any{}, messages...), map[string]any{"role": "user", "content": "they do! increase the timing"}))
	bannered := toolTail()
	if !closeInterruptedToolSequence(&bannered, "Response truncated.", false) {
		t.Fatal("bannered close returned false")
	}
	last := bannered[len(bannered)-1]
	if last["content"] != "Response truncated." {
		t.Fatalf("banner content %#v", last["content"])
	}
	if _, ok := last["display_kind"]; ok {
		t.Fatal("banner row hidden")
	}
}

func TestAssistantTailIsLeftUntouched(t *testing.T) {
	messages := []map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "partial reply"}}
	before := []map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "partial reply"}}
	if closeInterruptedToolSequence(&messages, "interrupted", false) || !reflect.DeepEqual(messages, before) {
		t.Fatalf("assistant tail changed: %#v", messages)
	}
}

func TestUserTailIsLeftUntouched(t *testing.T) {
	messages := []map[string]any{{"role": "user", "content": "hi"}}
	if closeInterruptedToolSequence(&messages, nil, false) || len(messages) != 1 {
		t.Fatal("user tail changed")
	}
}

func TestOnlyAnInterruptedCloseCarriesTheStoppedMark(t *testing.T) {
	stopped := toolTail()
	if !closeInterruptedToolSequence(&stopped, nil, true) {
		t.Fatal("stopped close returned false")
	}
	if got := stopped[len(stopped)-1]["display_metadata"]; !reflect.DeepEqual(got, map[string]any{"interrupted": true}) {
		t.Fatalf("stopped mark %#v", got)
	}
	truncated := toolTail()
	if !closeInterruptedToolSequence(&truncated, "Response truncated.", false) {
		t.Fatal("truncated close returned false")
	}
	if _, ok := truncated[len(truncated)-1]["display_metadata"]; ok {
		t.Fatal("truncation carries the stopped mark")
	}
}

func TestInterruptMarkerCloseIsHidden(t *testing.T) {
	messages := toolTail()
	closeInterruptedToolSequence(&messages, "  "+InterruptedPlaceholder+" ", false)
	last := messages[len(messages)-1]
	if last["display_kind"] != "hidden" || last["content"] != "" || last["api_content"] != InterruptedPlaceholder {
		t.Fatalf("interrupt row %#v", last)
	}
}

func parsedJSON(t *testing.T, text string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("not JSON %q: %v", text, err)
	}
	return out
}

func TestRepairToolCallArguments(t *testing.T) {
	for _, raw := range []string{"", `{"truncated": "val`, "garbage no json"} {
		if got := provider.RepairToolCallArguments(raw, "t"); got != "{}" {
			t.Fatalf("%q repaired to %q", raw, got)
		}
	}
	cases := map[string]string{
		`{"a": [1, 2,]}`:               `{"a": [1, 2]}`,
		`{"code": "}", "x": 1`:         `{"code": "}", "x": 1}`,
		`{"items": [{"n": 1}, {"n": 2`: `{"items": [{"n": 1}, {"n": 2}]}`,
		`{"a": [{"b": 1}, {"c": 2}}]}`: `{"a": [{"b": 1}, {"c": 2}]}`,
		`{"edits": [{"path": "a.py", "mode": "w"}, {"path": "b.py", "mode": "w"}}`:               `{"edits": [{"path": "a.py", "mode": "w"}, {"path": "b.py", "mode": "w"}]}`,
		`{"tool": "edit", "args": {"items": [{"k": 1}, {"k": 2}}}}`:                              `{"tool": "edit", "args": {"items": [{"k": 1}, {"k": 2}]}}`,
		`{"calls": [{"name": "a", "arguments": {"x": 1}}, {"name": "b", "arguments": {"y": 2}}}`: `{"calls": [{"name": "a", "arguments": {"x": 1}}, {"name": "b", "arguments": {"y": 2}}]}`,
		`{"a": [1, 2}`: `{"a": [1, 2]}`,
	}
	for raw, want := range cases {
		if got := provider.RepairToolCallArguments(raw, "t"); !reflect.DeepEqual(parsedJSON(t, got), parsedJSON(t, want)) {
			t.Fatalf("%q repaired to %q, want %s", raw, got, want)
		}
	}
}

func TestSerializedMessagesBytes(t *testing.T) {
	if serializedMessagesBytes([]any{}) != 0 || serializedMessagesBytes("x") != 0 {
		t.Fatal("non list or empty must size zero")
	}
	msgs := []any{map[string]any{"role": "user", "content": "héllo"}}
	if got := serializedMessagesBytes(msgs); got != len(`[{"role":"user","content":"héllo"}]`) && got != len(`[{"content":"héllo","role":"user"}]`) {
		t.Fatalf("size %d", got)
	}
	bad := []any{map[string]any{"content": "a\xed\xa0\x80"}}
	if got := serializedMessagesBytes(bad); got != len("{'content': 'a\\ud800'}") {
		t.Fatalf("surrogate fallback size %d", got)
	}
}

func TestImageRejectionPhrases(t *testing.T) {
	if !looksLikeImageContentRejection("HTTP 404: No endpoints found that support image input") || looksLikeImageContentRejection(nil) {
		t.Fatal("capability rejection")
	}
	if !looksLikeCorruptImageRejection("prepare image failed: failed to decode image") || looksLikeCorruptImageRejection("does not support images") {
		t.Fatal("corrupt rejection")
	}
}

func TestApplyReasoningContentPolicy(t *testing.T) {
	src := map[string]any{"role": "assistant", "reasoning": "think", "reasoning_details": []any{map[string]any{"type": "x.native_assistant"}, map[string]any{"type": "other"}}}
	api := map[string]any{"role": "assistant", "reasoning": "think", "reasoning_details": src["reasoning_details"]}
	applyReasoningContentPolicy(src, api, false, map[string]bool{carrierRC: true})
	want := map[string]any{"role": "assistant", "reasoning_content": "think", "reasoning_details": []any{map[string]any{"type": "x.native_assistant"}}}
	if !reflect.DeepEqual(api, want) {
		t.Fatalf("policy %#v", api)
	}
	native := map[string]any{"role": "assistant", "reasoning": "r", "tool_calls": []any{map[string]any{"id": "c"}}}
	out := map[string]any{"role": "assistant"}
	applyReasoningContentPolicy(native, out, true, nil)
	if out[carrierRC] != " " {
		t.Fatalf("legacy pad %#v", out)
	}
	strict := []any{map[string]any{"role": "assistant", "reasoning_content": "a", "reasoning": "a"}, map[string]any{"role": "user"}}
	if n := reapplyReasoningEcho(strict, false, map[string]bool{}); n != 1 || len(strict[0].(map[string]any)) != 1 {
		t.Fatalf("strict strip changed %d: %#v", n, strict[0])
	}
	if n := reapplyReasoningEcho(strict, false, map[string]bool{}); n != 0 {
		t.Fatal("not idempotent")
	}
}

func TestCoalesceReadsStructToolCalls(t *testing.T) {
	tc := state.ToolCall{ID: " call_a|fc_1 ", Extra: map[string]any{"call_id": "call_b"}}
	if got := state.CoalesceToolCallID(tc); got != "call_b" {
		t.Fatalf("struct coalesce %q", got)
	}
	if got := state.CoalesceToolCallID(&state.ToolCall{ID: " call_a|fc_1 "}); got != "call_a" {
		t.Fatalf("pointer coalesce %q", got)
	}
}
