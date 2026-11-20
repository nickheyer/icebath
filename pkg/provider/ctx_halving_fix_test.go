package provider

import "testing"

func TestParseAvailableOutputTokensAnthropicCanonicalFormat(t *testing.T) {
	msg := "max_tokens: 32768 > context_window: 200000 - input_tokens: 190000 = available_tokens: 10000"
	if got, ok := ParseAvailableOutputTokensFromError(msg); !ok || got != 10000 {
		t.Fatalf("got %d %t", got, ok)
	}
}

func TestParseAvailableOutputTokensAvailableTokensNaturalLanguage(t *testing.T) {
	msg := "max_tokens must be at most 10000 given your prompt (available tokens: 10000)"
	if got, ok := ParseAvailableOutputTokensFromError(msg); !ok || got != 10000 {
		t.Fatalf("got %d %t", got, ok)
	}
}

func TestParseAvailableOutputTokensPromptTooLongIsNotOutputCapError(t *testing.T) {
	if _, ok := ParseAvailableOutputTokensFromError("prompt is too long: 205000 tokens > 200000 maximum"); ok {
		t.Fatal("prompt too long errors need context overflow recovery")
	}
}

func TestParseAvailableOutputTokensNoMaxTokensKeyword(t *testing.T) {
	if _, ok := ParseAvailableOutputTokensFromError("invalid_api_key: the API key is invalid"); ok {
		t.Fatal("an unrelated error carries no output budget")
	}
}

func TestContextOverflowLimitSelectionGenericOverflowWithoutProviderLimitKeepsContextLength(t *testing.T) {
	msg := "Your input exceeds the context window of this model. Please adjust your input and try again."
	if _, ok := ParseContextLimitFromError(msg); ok {
		t.Fatal("a generic overflow names no limit")
	}
	if _, ok := GetContextLengthFromProviderError(msg, 1_000_000); ok {
		t.Fatal("a generic overflow must not invent a lower window")
	}
}

func TestContextOverflowLimitSelectionExplicitProviderLimitStillSelectsThatLimit(t *testing.T) {
	if got, ok := GetContextLengthFromProviderError("prompt is too long: 300000 tokens > 272000 maximum", 1_000_000); !ok || got != 272_000 {
		t.Fatalf("got %d %t", got, ok)
	}
}

func TestContextOverflowLimitSelectionReportedLimitNotLowerThanCurrentIsIgnored(t *testing.T) {
	if _, ok := GetContextLengthFromProviderError("maximum context length is 1000000 tokens", 272_000); ok {
		t.Fatal("a limit at or above the current window is ignored")
	}
}
