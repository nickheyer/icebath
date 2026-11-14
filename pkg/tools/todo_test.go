package tools

import (
	"strings"
	"sync"
	"testing"
)

func todoItems(pairs ...string) []any {
	var out []any
	for i := 0; i+2 < len(pairs)+1 && i < len(pairs); i += 3 {
		item := map[string]any{"id": pairs[i], "content": pairs[i+1], "status": pairs[i+2]}
		out = append(out, item)
	}
	return out
}

func TestTodoWriteValidatesAndRevisions(t *testing.T) {
	s := NewTodoStore()
	if _, err := s.Write([]any{map[string]any{"id": "1", "content": " "}}, false); err == nil || !strings.Contains(err.Error(), "Todo item '1' has empty or missing content") {
		t.Fatal(err)
	}
	if s.Revision() != 0 || s.HasItems() {
		t.Fatal("rejected write must leave the list untouched")
	}
	items, err := s.Write(todoItems("1", "a", "pending", "2", "b", "in_progress", "1", "c", "weird"), false)
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	if items[0].ID != "2" || items[1].ID != "1" || items[1].Content != "c" || items[1].Status != "pending" {
		t.Fatalf("dedupe keeps last and lifts in_progress: %+v", items)
	}
	if s.Revision() != 1 {
		t.Fatal("revision")
	}
	if _, err := s.Write(todoItems("2", "b", "in_progress", "1", "c", "pending"), false); err != nil || s.Revision() != 1 {
		t.Fatal("same content must not bump the revision")
	}
	long := strings.Repeat("x", 5000)
	items, _ = s.Write([]any{map[string]any{"id": "9", "content": long, "status": "pending"}}, false)
	if len([]rune(items[0].Content)) != MaxTodoContentChars || !strings.HasSuffix(items[0].Content, todoTruncationMark) {
		t.Fatal("content cap")
	}
}

func TestTodoMergeAndParents(t *testing.T) {
	s := NewTodoStore()
	s.Write(todoItems("1", "a", "pending", "2", "b", "pending"), false)
	items, err := s.Write([]any{map[string]any{"id": "2", "status": "completed"}, map[string]any{"id": "3", "content": "c", "status": "pending", "parent": "1"}, map[string]any{"content": "no id"}}, true)
	if err != nil || len(items) != 3 || items[1].Status != "completed" || items[2].Parent != "1" {
		t.Fatalf("%+v %v", items, err)
	}
	items, _ = s.Write([]any{map[string]any{"id": "3", "parent": nil}}, true)
	if items[2].Parent != "" {
		t.Fatal("null parent must clear")
	}
	items, _ = s.Write([]any{map[string]any{"id": "a", "content": "x", "status": "pending", "parent": "b"}, map[string]any{"id": "b", "content": "y", "status": "pending", "parent": "a"}, map[string]any{"id": "c", "content": "z", "status": "pending", "parent": "ghost"}}, false)
	for _, it := range items {
		if it.Parent != "" && it.ID != "b" {
			t.Fatalf("cycle or dangling parent kept: %+v", items)
		}
	}
	big := make([]any, MaxTodoItems+5)
	for i := range big {
		big[i] = map[string]any{"id": jsonCompact(i), "content": "c", "status": "pending"}
	}
	if items, _ := s.Write(big, false); len(items) != MaxTodoItems {
		t.Fatal("item cap")
	}
}

func TestTodoFormatForInjection(t *testing.T) {
	s := NewTodoStore()
	if s.FormatForInjection() != "" {
		t.Fatal("empty list")
	}
	s.Write([]any{
		map[string]any{"id": "1", "content": "phase", "status": "completed"},
		map[string]any{"id": "1a", "content": "sub", "status": "in_progress", "parent": "1"},
		map[string]any{"id": "2", "content": "done", "status": "completed"},
		map[string]any{"id": "3", "content": "todo", "status": "pending"},
	}, false)
	got := s.FormatForInjection()
	want := TodoInjectionHeader + "\n- [x] 1. phase (completed)\n  - [>] 1a. sub (in_progress)\n- [ ] 3. todo (pending)"
	if got != want {
		t.Fatalf("\n%s\nwant\n%s", got, want)
	}
	s.Write(todoItems("1", "x", "completed"), false)
	if s.FormatForInjection() != "" {
		t.Fatal("finished only must inject nothing")
	}
}

func TestTodoRestoreAndTool(t *testing.T) {
	s := NewTodoStore()
	s.Restore(todoItems("1", "a", "pending"), "7")
	if s.Revision() != 7 || !s.HasItems() {
		t.Fatal("restore")
	}
	s.Restore(nil, "bad")
	if s.Revision() != 0 || s.HasItems() {
		t.Fatal("restore bad revision")
	}
	env := &Env{Values: &sync.Map{}}
	if r := todoTool(TodoStoreOf(env), nil, false); !strings.Contains(r, "TodoStore not initialized") {
		t.Fatal(r)
	}
	env.Values.Store(TodoToolName, s)
	if r := todoTool(TodoStoreOf(env), "nope", false); !strings.Contains(r, "todos must be a list, got str") {
		t.Fatal(r)
	}
	out := mustJSON(t, todoTool(s, todoItems("1", "a", "pending", "2", "b", "cancelled"), false))
	summary := out["summary"].(map[string]any)
	if summary["total"] != float64(2) || summary["cancelled"] != float64(1) || out["revision"] != float64(1) {
		t.Fatal(out)
	}
}

func TestIsTodoToolCall(t *testing.T) {
	if !IsTodoToolCall("todo", nil) || !IsTodoToolCall(TodoToolName, "{}") {
		t.Fatal("direct names")
	}
	if IsTodoToolCall(ToolCallName, `{"calls":[{"name":"read_file","arguments":{}}]}`) {
		t.Fatal("other bridge call")
	}
	if !IsTodoToolCall(ToolCallName, `{"calls":[{"name":"todo_list","arguments":{}}]}`) {
		t.Fatal("bridged todo call")
	}
	if IsTodoToolCall(ToolCallName, `{"calls":[{"name":"todo_list","arguments":{}},{"name":"todo_list","arguments":{}}]}`) {
		t.Fatal("bridge must wrap exactly one call")
	}
	if IsTodoToolCall(ToolCallName, "not json") || IsTodoToolCall("other", `{"name":"todo"}`) {
		t.Fatal("garbage")
	}
}
