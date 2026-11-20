package provider

import (
	"strings"
	"testing"
)

// OpenAI shaped chat completion whose only message carries the content and the extra message fields
func auxContentResponse(content any, extra map[string]any) map[string]any {
	message := map[string]any{"content": content, "tool_calls": nil}
	for k, v := range extra {
		message[k] = v
	}
	return map[string]any{"choices": []any{map[string]any{"message": message}}}
}

func TestExtractContentOrReasoningNormalContentReturned(t *testing.T) {
	if got := ExtractContentOrReasoning(auxContentResponse("  Hello world  ", nil), -1); got != "Hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractContentOrReasoningNoneContentReturnsEmpty(t *testing.T) {
	if got := ExtractContentOrReasoning(auxContentResponse(nil, nil), -1); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractContentOrReasoningThinkBlocksStrippedWithRemainingContent(t *testing.T) {
	if got := ExtractContentOrReasoning(auxContentResponse("<think>internal reasoning</think>The answer is 42.", nil), -1); got != "The answer is 42." {
		t.Fatalf("got %q", got)
	}
}

func TestExtractContentOrReasoningThinkOnlyContentFallsBackToReasoningField(t *testing.T) {
	resp := auxContentResponse("<think>some reasoning</think>", map[string]any{"reasoning": "The actual reasoning output"})
	if got := ExtractContentOrReasoning(resp, -1); got != "The actual reasoning output" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractContentOrReasoningContentPreferredOverReasoning(t *testing.T) {
	resp := auxContentResponse("Actual answer", map[string]any{"reasoning": "Internal reasoning"})
	if got := ExtractContentOrReasoning(resp, -1); got != "Actual answer" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractContentOrReasoningDictMessageAndWhitespaceFallBack(t *testing.T) {
	if got := ExtractContentOrReasoning(map[string]any{"content": " ", "reasoning_content": "dict reasoning"}, -1); got != "dict reasoning" {
		t.Fatalf("dict message got %q", got)
	}
	resp := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "", "reasoning": "from response"}}}}
	if got := ExtractContentOrReasoning(resp, -1); got != "from response" {
		t.Fatalf("response got %q", got)
	}
}

func TestExtractContentOrReasoningStringMessagePassthrough(t *testing.T) {
	resp := map[string]any{"choices": []any{map[string]any{"message": "plain summary text"}}}
	if got := ExtractContentOrReasoning(resp, -1); got != "plain summary text" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractContentOrReasoningReasoningFallbackRespectsMaxChars(t *testing.T) {
	huge := strings.Repeat("t", 20000)
	if got := ExtractContentOrReasoning(map[string]any{"content": "", "reasoning_content": huge}, 8000); got != huge[:8000] {
		t.Fatalf("capped length %d", len(got))
	}
	if got := ExtractContentOrReasoning(map[string]any{"content": "", "reasoning_content": "short"}, 8000); got != "short" {
		t.Fatalf("short got %q", got)
	}
}
