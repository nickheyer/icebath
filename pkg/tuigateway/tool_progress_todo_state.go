package tuigateway

import (
	"math/big"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Client safe full task plan snapshot of a decoded value, nil when malformed or the unused empty store
func NormalizeTodoState(value any) *pyjson.Object {
	obj, ok := value.(*pyjson.Object)
	if !ok || obj == nil {
		return nil
	}
	todos, ok := obj.GetOr("todos", nil).([]any)
	if !ok {
		return nil
	}
	raw := obj.GetOr("revision", nil)
	if !pyrepr.Truthy(raw) {
		raw = int64(0)
	}
	revision, err := pyint.Of(raw)
	if err != nil {
		return nil
	}
	if revision.Sign() < 0 {
		revision = big.NewInt(0)
	}
	if len(todos) == 0 && revision.Sign() == 0 {
		return nil
	}
	return pyjson.ObjectOf("todos", append([]any{}, todos...), "revision", pyint.Value(revision))
}

// Rows of a transcript field that is a list of dicts
func historyRows(value any) []map[string]any {
	switch rows := value.(type) {
	case []map[string]any:
		return rows
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if m, ok := pyjson.Dict(row); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Latest task plan snapshot of a loaded transcript, the newest result paired with an assistant plan tool call
func TodoStateFromHistory(history []map[string]any) *pyjson.Object {
	if len(history) == 0 {
		return nil
	}
	todoCallIDs := map[any]bool{}
	for _, msg := range history {
		for _, call := range historyRows(msg["tool_calls"]) {
			id := call["id"]
			if !pyrepr.Truthy(id) {
				continue
			}
			if !reflect.TypeOf(id).Comparable() {
				return nil
			}
			fn, _ := pyjson.Dict(call["function"])
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
		decoded, err := pyjson.Loads(content)
		if err != nil {
			continue
		}
		return NormalizeTodoState(decoded)
	}
	return nil
}
