package pycopy

import "testing"

type holder struct {
	Items []any
	Meta  map[string]any
	next  *holder
}

func TestDeepCopyDetachesNestedContainers(t *testing.T) {
	src := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}}}
	dup := Of(src)
	dup[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "changed"
	if got := src[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; got != "hi" {
		t.Fatalf("source mutated through the copy: %v", got)
	}
}

func TestDeepCopyKeepsSharedReferencesShared(t *testing.T) {
	shared := map[string]any{"k": 1}
	src := []any{shared, shared}
	dup := Of(src)
	dup[0].(map[string]any)["k"] = 2
	if dup[1].(map[string]any)["k"] != 2 {
		t.Fatal("aliased map was split into two copies")
	}
	if shared["k"] != 1 {
		t.Fatal("source mutated")
	}
}

func TestDeepCopyStructsAndPointers(t *testing.T) {
	src := &holder{Items: []any{"a"}, Meta: map[string]any{"x": []any{1}}}
	src.next = src
	dup := Of(src)
	dup.Items[0] = "b"
	dup.Meta["x"].([]any)[0] = 2
	if src.Items[0] != "a" || src.Meta["x"].([]any)[0] != 1 {
		t.Fatal("struct fields still shared")
	}
	if DeepCopy(nil) != nil {
		t.Fatal("nil copy")
	}
}
