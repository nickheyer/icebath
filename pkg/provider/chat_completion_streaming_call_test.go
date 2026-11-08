package provider

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Accumulator holding one edit call whose arguments streamed as the given text
func accumulatedEditCall(arguments string) *ccToolCallAccumulator {
	acc := newCCToolCallAccumulator()
	acc.Feed(map[string]any{"index": 0.0, "id": "call_1", "type": "function", "function": map[string]any{"name": "edit", "arguments": arguments}})
	return acc
}

func assembledArguments(t *testing.T, calls []map[string]any) string {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("calls %v", calls)
	}
	args, _ := calls[0]["function"].(map[string]any)["arguments"].(string)
	return args
}

func TestStreamedMisnestedArgumentsAreRepairedNotFlaggedTruncated(t *testing.T) {
	raw := `{"edits": [{"path": "a.py", "mode": "w"}, {"path": "b.py", "mode": "w"}}`
	calls, truncated := assembleStreamedToolCalls(accumulatedEditCall(raw), "tool_calls")
	if truncated {
		t.Fatal("repairable arguments flagged truncated")
	}
	var got any
	if err := json.Unmarshal([]byte(assembledArguments(t, calls)), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"edits": []any{map[string]any{"path": "a.py", "mode": "w"}, map[string]any{"path": "b.py", "mode": "w"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestStreamedMidStringCutStaysUnrepairable(t *testing.T) {
	calls, truncated := assembleStreamedToolCalls(accumulatedEditCall(`{"q": "unterminated string`), "")
	if !truncated {
		t.Fatal("mid string cut must stay flagged")
	}
	if got := assembledArguments(t, calls); got != `{"q": "unterminated string` {
		t.Fatalf("arguments %q", got)
	}
}

func TestStreamingAssemblyRepairTruncatedObjectNoCloseBrace(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(RepairToolCallArguments(`{"command": "ls -la", "timeout": 30`, "terminal")), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["command"] != "ls -la" || parsed["timeout"] != 30.0 {
		t.Fatalf("parsed %v", parsed)
	}
}

func TestStreamingAssemblyRepairEmptyString(t *testing.T) {
	if got := RepairToolCallArguments("", "test"); got != "{}" {
		t.Fatalf("got %q", got)
	}
}
