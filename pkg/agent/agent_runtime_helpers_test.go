package agent

import (
	"reflect"
	"testing"
)

func orphanCall(id, name string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": "{}"}}
}

func TestClassifyOrphansCleanPair(t *testing.T) {
	messages := []map[string]any{
		{"role": "assistant", "tool_calls": []any{orphanCall("call_1", "f")}},
		{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
	}
	sv, rs, orphaned, missing := classifyToolCallOrphans(messages)
	if !reflect.DeepEqual(sv, map[string]bool{"call_1": true}) || !reflect.DeepEqual(rs, map[string]bool{"call_1": true}) {
		t.Fatalf("sets %v %v", sv, rs)
	}
	if len(orphaned) != 0 || len(missing) != 0 {
		t.Fatalf("orphaned %v missing %v", orphaned, missing)
	}
}

func TestClassifyOrphansMixed(t *testing.T) {
	messages := []map[string]any{
		{"role": "assistant", "tool_calls": []any{orphanCall("call_A", "f"), orphanCall("call_B", "g")}},
		{"role": "tool", "tool_call_id": "call_A", "content": "ok"},
		{"role": "tool", "tool_call_id": "call_C", "content": "orphan"},
	}
	sv, rs, orphaned, missing := classifyToolCallOrphans(messages)
	if !reflect.DeepEqual(sv, map[string]bool{"call_A": true, "call_B": true}) {
		t.Fatalf("surviving %v", sv)
	}
	if !reflect.DeepEqual(rs, map[string]bool{"call_A": true, "call_C": true}) {
		t.Fatalf("results %v", rs)
	}
	if len(orphaned) != 1 || orphaned[0]["tool_call_id"] != "call_C" {
		t.Fatalf("orphaned %v", orphaned)
	}
	if len(missing) != 1 || missing[0].(map[string]any)["id"] != "call_B" {
		t.Fatalf("missing %v", missing)
	}
}
