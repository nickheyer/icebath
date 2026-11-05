package state

import (
	"reflect"
	"testing"
)

func TestSanitizeSurrogatesAndNonASCII(t *testing.T) {
	if got := SanitizeSurrogates("a\xed\xa0\x80b\xed\xb0\x80é"); got != "a�b�é" {
		t.Fatalf("surrogates %q", got)
	}
	if got := SanitizeSurrogates("plain"); got != "plain" {
		t.Fatal("ascii changed")
	}
	if got := StripNonASCII("héllo ☃"); got != "hllo " {
		t.Fatalf("non ascii %q", got)
	}
}

func TestSanitizeMessagesDeepAndShallow(t *testing.T) {
	bad := "x\xed\xa0\x80"
	msg := map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": bad}},
		"tool_calls":        []any{map[string]any{"id": bad, "function": map[string]any{"name": bad, "arguments": bad}}},
		"reasoning_details": []any{map[string]any{"text": bad}},
		"reasoning":         bad, DBPersistedMarker: true, DBRowSnapshot: bad,
	}
	if !SanitizeMessagesSurrogates([]any{msg, "skip"}) {
		t.Fatal("nothing fixed")
	}
	fixed := "x�"
	tc := msg["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != fixed || tc["function"].(map[string]any)["name"] != fixed || msg["reasoning_details"].([]any)[0].(map[string]any)["text"] != fixed || msg["reasoning"] != fixed {
		t.Fatalf("deep sweep %#v", msg)
	}
	if _, ok := msg[DBPersistedMarker]; ok || msg[DBRowSnapshot] != bad {
		t.Fatal("marker kept or snapshot rewritten")
	}
	shallow := map[string]any{"role": "user", "content": "é", "tool_calls": []any{map[string]any{"id": "é", "function": map[string]any{"name": "é", "arguments": "é"}}}, "nested": []any{"é"}}
	SanitizeMessagesNonASCII([]any{shallow})
	tc = shallow["tool_calls"].([]any)[0].(map[string]any)
	if shallow["content"] != "" || tc["id"] != "é" || tc["function"].(map[string]any)["name"] != "é" || tc["function"].(map[string]any)["arguments"] != "" || shallow["nested"].([]any)[0] != "é" {
		t.Fatalf("shallow sweep %#v", shallow)
	}
}

func TestToolCallSetAndExtraRead(t *testing.T) {
	tc := &ToolCall{ID: "a"}
	if err := ToolCallSet(tc, "id", "b"); err != nil || tc.ID != "b" {
		t.Fatal("id field")
	}
	if err := ToolCallSet(tc, "call_id", "c"); err != nil || ToolCallField(tc, "call_id") != "c" || CoalesceToolCallID(tc) != "c" {
		t.Fatal("extra field")
	}
	if ToolCallSet(ToolCall{}, "id", "x") == nil {
		t.Fatal("value struct accepted a write")
	}
}

func TestMessageDictRoundTrip(t *testing.T) {
	d := map[string]any{
		"role": "assistant", "content": "hi", "tool_call_id": "",
		"tool_calls":        []any{map[string]any{"id": "c1", "call_id": "c1", "function": map[string]any{"name": "f", "arguments": "{}"}}},
		"reasoning_details": []any{map[string]any{"type": "t"}},
		"_withheld_prompt":  "w", "data_use": []any{"x"}, "timestamp": 1.5, "id": int64(7), DBPersistedMarker: true,
	}
	m := MessageFromDict(d)
	if m.Role != "assistant" || m.RowID != 7 || !m.Persisted || len(m.ToolCalls) != 1 || m.ToolCalls[0].Extra["call_id"] != "c1" || m.Extra["_withheld_prompt"] != "w" {
		t.Fatalf("struct %#v", m)
	}
	if back := MessageToDict(m); !reflect.DeepEqual(back, d) {
		t.Fatalf("round trip %#v", back)
	}
	noContent := map[string]any{"role": "user"}
	if back := MessageToDict(MessageFromDict(noContent)); !reflect.DeepEqual(back, noContent) {
		t.Fatalf("absent content %#v", back)
	}
}
