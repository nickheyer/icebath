package state

import (
	"context"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Restored content and reasoning details are plain dicts and restored tool calls keep their stored key order
func TestRestoredConversationCarriesPlainPartsAndOrderedToolCalls(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "sess1", Source: "test"})
	parts := []any{map[string]any{"type": "text", "text": "look"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}}
	readAppend(t, db, "sess1", map[string]any{"role": "user", "content": parts})
	readAppend(t, db, "sess1", map[string]any{"role": "assistant", "content": "",
		"tool_calls": []any{pyjson.ObjectOf("id", "call_1", "type", "function",
			"function", pyjson.ObjectOf("name", "read_file", "arguments", "{\"path\":\"a\"}"))},
		"reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": "t"}}})
	readAppend(t, db, "sess1", map[string]any{"role": "tool", "content": "ok", "tool_call_id": "call_1"})
	for _, repair := range []bool{false, true} {
		msgs, err := db.GetMessagesAsConversation(context.Background(), "sess1", ConversationOptions{RepairAlternation: repair})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Fatalf("restored %d messages", len(msgs))
		}
		for i, m := range msgs {
			for _, key := range []string{"content", "reasoning_details"} {
				if !reflectPlain(m[key], true) {
					t.Fatalf("repair=%v message %d %s holds an ordered object: %#v", repair, i, key, m[key])
				}
			}
			if !reflectPlain(m["tool_calls"], false) {
				t.Fatalf("repair=%v message %d tool_calls holds a decoder object: %#v", repair, i, m["tool_calls"])
			}
		}
		content, _ := msgs[0]["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("content %#v", msgs[0]["content"])
		}
		image, _ := content[1].(map[string]any)
		url, _ := image["image_url"].(map[string]any)
		if url["url"] != "data:image/png;base64,AAAA" {
			t.Fatalf("image part %#v", content[1])
		}
		calls, _ := msgs[1]["tool_calls"].([]any)
		if len(calls) != 1 {
			t.Fatalf("tool calls %#v", msgs[1]["tool_calls"])
		}
		call, _ := calls[0].(*pyjson.Object)
		if call == nil || !slices.Equal(call.Keys, []string{"id", "type", "function"}) {
			t.Fatalf("tool call %#v", calls[0])
		}
		fn, _ := call.Vals["function"].(*pyjson.Object)
		if fn == nil || !slices.Equal(fn.Keys, []string{"name", "arguments"}) || fn.Vals["name"] != "read_file" {
			t.Fatalf("tool call function %#v", call.Vals["function"])
		}
	}
}

// True when no decoder object hides anywhere in the value, and no ordered object either when strict
func reflectPlain(v any, strict bool) bool {
	switch t := v.(type) {
	case pyObject:
		return false
	case *pyjson.Object:
		if strict || t == nil {
			return !strict
		}
		for _, item := range t.Vals {
			if !reflectPlain(item, strict) {
				return false
			}
		}
	case map[string]any:
		for _, item := range t {
			if !reflectPlain(item, strict) {
				return false
			}
		}
	case []any:
		for _, item := range t {
			if !reflectPlain(item, strict) {
				return false
			}
		}
	}
	return true
}
