package gatewayapi

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// One tool round trip then the final answer of the turn
func appendToolTurn(messages *[]map[string]any, callID, answer string) {
	state.AppendMessage(messages, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}}, nil)
	state.AppendMessage(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": "ok"}, nil)
	state.AppendMessage(messages, map[string]any{"role": "assistant", "content": answer, "finish_reason": "stop"}, nil)
}

func rowsAsAny(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

// Key of a stored row whether it is a plain map or an ordered object
func rowField(row any, key string) any {
	switch r := row.(type) {
	case map[string]any:
		return r[key]
	case *pyjson.Object:
		v, _ := r.Get(key)
		return v
	}
	return nil
}

func TestRepairedEarlierRowsKeepOnlyThisTurnAsOutput(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	history := []map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "hello"},
		{"role": "assistant", "content": "again"}}
	prompt := "Q1: run the tool"
	messages := append([]map[string]any(nil), history...)
	state.AppendMessage(&messages, map[string]any{"role": "user", "content": prompt}, nil)
	if _, err := state.RepairMessageSequence(&messages); err != nil {
		t.Fatal(err)
	}
	appendToolTurn(&messages, "call-current", "done")
	result := map[string]any{"messages": rowsAsAny(messages)}
	start := responseMessagesTurnStartIndex(rowsAsAny(history), prompt, result)
	items, err := extractOutputItems(result, start, "")
	if err != nil {
		t.Fatal(err)
	}
	if messages[start-1]["content"] != prompt {
		t.Fatalf("turn starts after %v", messages[start-1])
	}
	var types []any
	for _, item := range items {
		types = append(types, rowField(item, "type"))
	}
	if !reflect.DeepEqual(types, []any{"function_call", "function_call_output", "message"}) {
		t.Fatalf("items %v", types)
	}
	stored := buildResponseConversationHistory(rowsAsAny(history), prompt, result, "done", 0)
	var roles []any
	for _, m := range stored {
		roles = append(roles, rowField(m, "role"))
	}
	if !reflect.DeepEqual(roles, []any{"user", "assistant", "user", "assistant", "tool", "assistant"}) {
		t.Fatalf("stored roles %v", roles)
	}
}
