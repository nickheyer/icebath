package state

import (
	"context"
	"testing"
)

// Every JSON object restored from stored rows reaches the live conversation as a plain dict
func TestRestoredConversationCarriesPlainDicts(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "sess1", Source: "test"})
	parts := []any{map[string]any{"type": "text", "text": "look"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}}
	readAppend(t, db, "sess1", map[string]any{"role": "user", "content": parts})
	readAppend(t, db, "sess1", map[string]any{"role": "assistant", "content": "",
		"tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
			"function": map[string]any{"name": "read_file", "arguments": "{\"path\":\"a\"}"}}},
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
			for _, key := range []string{"content", "tool_calls", "reasoning_details"} {
				if !reflectPlain(m[key]) {
					t.Fatalf("repair=%v message %d %s holds an ordered object: %#v", repair, i, key, m[key])
				}
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
		call, _ := calls[0].(map[string]any)
		fn, _ := call["function"].(map[string]any)
		if len(calls) != 1 || fn["name"] != "read_file" {
			t.Fatalf("tool calls %#v", msgs[1]["tool_calls"])
		}
	}
}

// True when no ordered object hides anywhere in the value
func reflectPlain(v any) bool {
	switch t := v.(type) {
	case pyObject:
		return false
	case map[string]any:
		for _, item := range t {
			if !reflectPlain(item) {
				return false
			}
		}
	case []any:
		for _, item := range t {
			if !reflectPlain(item) {
				return false
			}
		}
	}
	return true
}
