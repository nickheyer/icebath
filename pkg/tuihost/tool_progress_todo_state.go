package tuihost

import (
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Client safe full task plan snapshot of a decoded value, nil when malformed or the unused empty store
func NormalizeTodoState(value any) *jsonutil.Object {
	obj, ok := value.(*jsonutil.Object)
	if !ok || obj == nil {
		return nil
	}
	todos, ok := obj.ValueOr("todos", nil).([]any)
	if !ok {
		return nil
	}
	raw := obj.ValueOr("revision", nil)
	if !anyutil.IsTrue(raw) {
		raw = int64(0)
	}
	revision, err := anyutil.ToInt(raw)
	if err != nil {
		return nil
	}
	if revision < 0 {
		revision = 0
	}
	if len(todos) == 0 && revision == 0 {
		return nil
	}
	return jsonutil.ObjectOf("todos", append([]any{}, todos...), "revision", revision)
}

// Rows of a transcript field that is a list of dicts
func historyRows(value any) []map[string]any {
	switch rows := value.(type) {
	case []map[string]any:
		return rows
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if m, ok := jsonutil.AsMap(row); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Latest task plan snapshot of a loaded transcript, the newest result paired with an assistant plan tool call
func TodoStateFromHistory(history []map[string]any) *jsonutil.Object {
	if len(history) == 0 {
		return nil
	}
	todoCallIDs := map[any]bool{}
	for _, msg := range history {
		for _, call := range historyRows(msg["tool_calls"]) {
			id := call["id"]
			if !anyutil.IsTrue(id) {
				continue
			}
			if !reflect.TypeOf(id).Comparable() {
				return nil
			}
			fn, _ := jsonutil.AsMap(call["function"])
			name, _ := fn["name"].(string)
			if tools.IsTodoToolCall(name, fn["arguments"]) {
				todoCallIDs[id] = true
			}
		}
	}
	if len(todoCallIDs) == 0 {
		return nil
	}
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg == nil || msg["role"] != "tool" {
			continue
		}
		if id := msg["tool_call_id"]; id != nil && !reflect.TypeOf(id).Comparable() {
			return nil
		}
		if !todoCallIDs[msg["tool_call_id"]] {
			continue
		}
		raw, present := msg["content"]
		if !present {
			raw = ""
		}
		content, isText := raw.(string)
		if !isText || utf8.RuneCountInString(content) > tools.MaxTodoResultChars || !strings.Contains(content, `"todos"`) {
			continue
		}
		decoded, err := jsonutil.Decode(content)
		if err != nil {
			continue
		}
		return NormalizeTodoState(decoded)
	}
	return nil
}
