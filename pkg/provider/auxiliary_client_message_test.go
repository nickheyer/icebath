package provider

import "testing"

func TestFirstChoiceMessageIndexesChoicesLikePython(t *testing.T) {
	msg := map[string]any{"content": "hi"}
	if got, err := FirstChoiceMessage(map[string]any{"choices": []any{map[string]any{"message": msg}}}); err != nil || got.(map[string]any)["content"] != "hi" {
		t.Fatalf("message %#v %v", got, err)
	}
	for _, c := range []struct {
		resp any
		want string
	}{
		{"text", "'str' object has no attribute 'choices'"},
		{map[string]any{"choices": []any{}}, "list index out of range"},
		{map[string]any{"choices": []any{map[string]any{}}}, "'dict' object has no attribute 'message'"},
	} {
		if _, err := FirstChoiceMessage(c.resp); err == nil || err.Error() != c.want {
			t.Fatalf("FirstChoiceMessage(%#v) error %v", c.resp, err)
		}
	}
}

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
