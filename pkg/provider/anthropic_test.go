package provider

import "testing"

func TestAnthropicMaxOutputExportReadsTheFamilyLimit(t *testing.T) {
	if AnthropicMaxOutput("claude-opus-4.6") != 128_000 || AnthropicMaxOutput("claude-sonnet-4-20250514") != 64_000 {
		t.Fatal("anthropic max output", AnthropicMaxOutput("claude-opus-4.6"))
	}
}

func TestIsMandatoryThinkingClaude(t *testing.T) {
	for model, want := range map[string]bool{"claude-opus-5-5": true, "Claude-Fable-1": true, "claude-sonnet-4-6": false, "gpt-5": false} {
		if got := IsMandatoryThinkingClaude(model); got != want {
			t.Fatalf("%s got %v", model, got)
		}
	}
}
