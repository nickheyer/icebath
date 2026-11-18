package tuigateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestStoredToolCallsDecodedAsOrderedObjectsNameTheirToolRows(t *testing.T) {
	call := pyjson.ObjectOf("id", "call_1", "type", "function",
		"function", pyjson.ObjectOf("name", "read_file", "arguments", `{"path": "notes.md"}`))
	history := []any{
		map[string]any{"role": "user", "content": "read the notes"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "notes"},
		map[string]any{"role": "assistant", "content": "done"},
	}
	messages, err := HistoryToMessages(context.Background(), history, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var tool map[string]any
	for _, raw := range messages {
		if m, _ := raw.(map[string]any); m["role"] == "tool" {
			tool = m
		}
	}
	if tool == nil || tool["name"] != "read_file" {
		t.Fatalf("tool row %v in %v", tool, messages)
	}
}
