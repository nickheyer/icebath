package provider

import "testing"

// Chat completion chunk carrying one choice with the given delta
func byteEstimateChunk(delta map[string]any) map[string]any {
	return map[string]any{"id": "chatcmpl-1", "created": 1730000000, "model": "m", "object": "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
}

func TestReasoningContentCounted(t *testing.T) {
	base := estimateChunkBytes(byteEstimateChunk(map[string]any{}))
	withReasoning := byteEstimateChunk(map[string]any{"content": nil, "reasoning_content": "thinking...", "reasoning": nil, "tool_calls": nil})
	if got := estimateChunkBytes(withReasoning); got != base+len("thinking...") {
		t.Fatalf("got %d want %d", got, base+len("thinking..."))
	}
}

func TestUnknownShapeReturnsFloorNeverRaises(t *testing.T) {
	floor := estimateChunkBytes(nil)
	if floor <= 0 {
		t.Fatalf("floor %d", floor)
	}
	for _, weird := range []map[string]any{{}, {"weird": true}, {"choices": "not a list"}} {
		if got := estimateChunkBytes(weird); got != floor {
			t.Fatalf("%v -> %d want %d", weird, got, floor)
		}
	}
}
