package tuigateway

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestTheHomeModuleReadsTheSnapshotWhenImportedDirectly(t *testing.T) {
	todos := []any{
		pyjson.ObjectOf("id", "1", "content", "write the fix", "status", "completed"),
		pyjson.ObjectOf("id", "2", "content", "ship it", "status", "in_progress"),
	}
	content, err := pyjson.Dumps(pyjson.ObjectOf("todos", todos, "revision", int64(2)))
	if err != nil {
		t.Fatal(err)
	}
	call := map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "todo_list", "arguments": "{}"}}
	history := []map[string]any{
		{"role": "assistant", "content": "", "tool_calls": []any{call}},
		{"role": "tool", "tool_call_id": "call-1", "content": content},
	}
	want := pyjson.ObjectOf("todos", todos, "revision", int64(2))
	if got := TodoStateFromHistory(history); !pyjson.Equal(got, want) {
		t.Fatalf("todo state %v, want %v", got, want)
	}
}
