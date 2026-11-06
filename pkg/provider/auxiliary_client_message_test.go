package provider

import "testing"

func TestExtractContentOrReasoningOrderAndCap(t *testing.T) {
	resp := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "<think>plan</think>  Final answer "}}}}
	if got := ExtractContentOrReasoning(resp, -1); got != "Final answer" {
		t.Fatalf("content %q", got)
	}
	thinkOnly := map[string]any{"content": "<THINKING>only</THINKING>", "reasoning": "r1", "reasoning_content": "r1",
		"reasoning_details": []any{map[string]any{"summary": "s1"}, map[string]any{"text": "t2"}, "skip"}}
	if got := ExtractContentOrReasoning(thinkOnly, -1); got != "r1\n\ns1\n\nt2" {
		t.Fatalf("reasoning fallback %q", got)
	}
	if got := ExtractContentOrReasoning(thinkOnly, 3); got != "r1\n" {
		t.Fatalf("capped %q", got)
	}
	if got := ExtractContentOrReasoning("  bare  ", -1); got != "bare" {
		t.Fatalf("bare string %q", got)
	}
	if got := ExtractContentOrReasoning(nil, -1); got != "" {
		t.Fatalf("nil %q", got)
	}
	if got := ExtractContentOrReasoning(map[string]any{"content": nil}, 10); got != "" {
		t.Fatalf("empty %q", got)
	}
}
