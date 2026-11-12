package pyjson

import (
	"iter"
	"testing"
)

type orderedPairs [][2]any

func (o orderedPairs) PyItems() iter.Seq2[any, any] {
	return func(yield func(any, any) bool) {
		for _, kv := range o {
			if !yield(kv[0], kv[1]) {
				return
			}
		}
	}
}

func TestDumpsEncodesADictLikeValueInItsOwnKeyOrder(t *testing.T) {
	value := orderedPairs{{"role", "user"}, {"content", orderedPairs{{"b", 1}, {"a", []any{true, nil}}}}, {2, "two"}, {nil, "none"}}
	got, err := Dumps(value)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"role": "user", "content": {"b": 1, "a": [true, null]}, "2": "two", "null": "none"}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	sorted, err := Dumps(orderedPairs{{"b", 1}, {"a", 2}}, SortKeys(true))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a": 2, "b": 1}`; sorted != want {
		t.Fatalf("got %s, want %s", sorted, want)
	}
	if _, err := Dumps(orderedPairs{{[]any{1}, "list key"}}); err == nil {
		t.Fatal("a list key was encoded")
	}
}

func TestOrderedTurnsEveryStringKeyedDictIntoAnObject(t *testing.T) {
	value := map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"usage":    orderedPairs{{"output", 2}, {"input", 1}},
		"nested":   ObjectOf("z", map[string]any{"b": 1, "a": []string{"x"}}),
		"blob":     []byte("raw"),
		"codes":    map[int]string{1: "one"},
	}
	out, isObject := Ordered(value).(*Object)
	if !isObject {
		t.Fatalf("result is %T", Ordered(value))
	}
	if blob, kept := out.Pop("blob"); !kept || string(blob.([]byte)) != "raw" {
		t.Fatalf("blob = %#v", blob)
	}
	got, err := Dumps(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"codes": {"1": "one"}, "messages": [{"content": "hi", "role": "user"}], ` +
		`"nested": {"z": {"a": ["x"], "b": 1}}, "usage": {"output": 2, "input": 1}}`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	rows, _ := out.GetOr("messages", nil).([]any)
	if _, isRow := rows[0].(*Object); len(rows) != 1 || !isRow {
		t.Fatalf("messages = %#v", out.GetOr("messages", nil))
	}
	if _, kept := out.GetOr("codes", nil).(map[int]string); !kept {
		t.Fatalf("codes = %#v", out.GetOr("codes", nil))
	}
}
