package provider

import (
	"encoding/json"
	"testing"
)

// A two key functionCall keeps the response key order and json.dumps separators in its arguments string
func TestGeminiCallArgsKeepResponseOrder(t *testing.T) {
	line := `data: {"candidates": [{"content": {"parts": [{"functionCall": {"name": "lookup", "args": {"zeta": 1.0, "alpha": {"y": 2, "x": "é"}}}}]}}]}`
	chunk, _ := parseGeminiSSELine(line)
	if chunk == nil {
		t.Fatal("chunk not parsed")
	}
	part := chunk["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)
	fc := part["functionCall"].(map[string]any)
	if got, want := geminiDumpCallArgs(fc), `{"zeta": 1.0, "alpha": {"y": 2, "x": "é"}}`; got != want {
		t.Fatalf("arguments %s, want %s", got, want)
	}
	if fc["name"] != "lookup" {
		t.Fatalf("name %v", fc["name"])
	}
}

// Replayed tool call arguments and object tool results keep their key order and Python number spelling
func TestGeminiRequestArgsKeepKeyOrder(t *testing.T) {
	tc := map[string]any{"id": "c1", "function": map[string]any{"name": "lookup", "arguments": `{"zeta": 1.0, "alpha": {"y": 2, "x": "é"}, "n": 1e16}`}}
	call := translateToolCallToGemini(tc, false)["functionCall"].(map[string]any)
	got, err := json.Marshal(call["args"])
	if want := `{"zeta":1.0,"alpha":{"y":2,"x":"é"},"n":1e+16}`; err != nil || string(got) != want {
		t.Fatalf("args %s %v, want %s", got, err, want)
	}
	tc["function"].(map[string]any)["arguments"] = `[2.50, 1]`
	call = translateToolCallToGemini(tc, false)["functionCall"].(map[string]any)
	if got, _ := json.Marshal(call["args"]); string(got) != `{"_value":[2.5,1]}` {
		t.Fatalf("wrapped args %s", got)
	}
	msg := map[string]any{"role": "tool", "tool_call_id": "c1", "name": "lookup", "content": `{"b": 1, "a": 0.5}`}
	fr := translateToolResultToGemini(msg, nil, false, false)["functionResponse"].(map[string]any)
	if got, _ := json.Marshal(fr["response"]); string(got) != `{"b":1,"a":0.5}` {
		t.Fatalf("response %s", got)
	}
}
