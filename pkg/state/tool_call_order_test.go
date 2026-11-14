package state

import (
	"context"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestToolCallsColumnKeepsBuildOrder(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	d := usageDB(t)
	ctx := context.Background()
	if err := d.CreateSession(NewSession{ID: "order", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	call := pyjson.ObjectOf("id", "call_1", "call_id", "call_1", "response_item_id", "fc_1", "type", "function",
		"function", pyjson.ObjectOf("name", "terminal", "arguments", `{"command":"echo hello"}`))
	row := MessageToDict(MessageFromDict(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call}}))
	if _, err := d.AppendMessage(ctx, "order", row, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	rows, err := d.readAll(ctx, "SELECT tool_calls FROM messages WHERE session_id = ?", "order")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v %v", rows, err)
	}
	want := `[{"id": "call_1", "call_id": "call_1", "response_item_id": "fc_1", "type": "function", "function": {"name": "terminal", "arguments": "{\"command\":\"echo hello\"}"}}]`
	if got, _ := rows[0].Get("tool_calls"); got != want {
		t.Fatalf("stored %v", got)
	}
	history, err := d.GetMessagesAsConversation(ctx, "order", ConversationOptions{})
	if err != nil || len(history) != 1 {
		t.Fatalf("history %v %v", history, err)
	}
	calls, _ := history[0]["tool_calls"].([]any)
	replayed, ok := calls[0].(*pyjson.Object)
	if !ok || !slices.Equal(replayed.Keys, call.Keys) {
		t.Fatalf("replayed %#v", calls)
	}
	again := MessageToDict(MessageFromDict(history[0]))["tool_calls"].([]any)[0].(*pyjson.Object)
	if text, _ := pyjson.Dumps(again); text != want[1:len(want)-1] {
		t.Fatalf("round trip %s", text)
	}
}
