package provider

import "testing"

func TestAnthropicMaxOutputExportReadsTheFamilyLimit(t *testing.T) {
	if AnthropicMaxOutput("claude-opus-4.6") != 128_000 || AnthropicMaxOutput("claude-sonnet-4-20250514") != 64_000 {
		t.Fatal("anthropic max output", AnthropicMaxOutput("claude-opus-4.6"))
	}
}
