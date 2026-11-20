package provider

import (
	"fmt"
	"testing"
)

// Parsed available output tokens, failing when the message carries none
func availableOutputTokens(t *testing.T, msg string) int {
	t.Helper()
	n, ok := ParseAvailableOutputTokensFromError(msg)
	if !ok {
		t.Fatalf("no available output tokens parsed from %q", msg)
	}
	return n
}

func TestParseOpenRouterOutputCapOpenrouterBreakdownFormat(t *testing.T) {
	msg := "This endpoint's maximum context length is 200000 tokens. " +
		"However, you requested about 195000 tokens " +
		"(150000 of text input, 40000 of tool input, 5000 in the output)."
	if got := availableOutputTokens(t, msg); got != 10000 {
		t.Fatalf("got %d", got)
	}
}

func TestParseCharBasedOutputCapCharBasedOutputCapFormat(t *testing.T) {
	msg := "This model's maximum context length is 65536 tokens. However, " +
		"you requested 65536 output tokens and your prompt contains " +
		"77409 characters (more than 0 characters, which is the upper " +
		"bound for 0 input tokens). Please reduce the length of the " +
		"input prompt or the number of requested output tokens."
	if got := availableOutputTokens(t, msg); got != 39733 {
		t.Fatalf("got %d", got)
	}
}

func TestParseCharBasedOutputCapCharBasedLeavesRoomForInput(t *testing.T) {
	ctx, chars := 65536, 77409
	available := availableOutputTokens(t, fmt.Sprintf("maximum context length is %d tokens. However, you requested "+
		"%d output tokens and your prompt contains %d characters.", ctx, ctx, chars))
	if available+(chars+2)/3 > ctx {
		t.Fatalf("available %d leaves no room for the input", available)
	}
}

func TestParseDashScopeOutputCapAnthropicOutputCeilingFormat(t *testing.T) {
	msg := "max_tokens: 100000 > 64000, which is the maximum allowed number of output tokens for claude-sonnet-4-5"
	if got := availableOutputTokens(t, msg); got != 64000 {
		t.Fatalf("got %d", got)
	}
}

func TestParseDashScopeOutputCapDashscopeRangeFormat(t *testing.T) {
	msg := "HTTP 400: InternalError.Algo.InvalidParameter: Range of max_tokens should be [1, 65536]"
	if got := availableOutputTokens(t, msg); got != 65536 {
		t.Fatalf("got %d", got)
	}
}

func TestParseDashScopeOutputCapDashscopeRangeWithSpaces(t *testing.T) {
	if got := availableOutputTokens(t, "range of max_tokens should be [ 1 , 32768 ]"); got != 32768 {
		t.Fatalf("got %d", got)
	}
}

func TestParseMaximumOutputTokensCapParenthesizedMaxOutputCap(t *testing.T) {
	msg := "API call failed after 3 retries: [400]: max_tokens (98304) exceeds model's maximum output tokens (65536)"
	if got := availableOutputTokens(t, msg); got != 65536 {
		t.Fatalf("got %d", got)
	}
}

func TestParseMaximumOutputTokensCapParenthesizedMaxOutputCapIsOutputCap(t *testing.T) {
	if !IsOutputCapError("max_tokens (98304) exceeds model's maximum output tokens (65536)") {
		t.Fatal("parenthesized cap is an output cap")
	}
}

func TestIsOutputCapErrorDashscopeIsOutputCap(t *testing.T) {
	if !IsOutputCapError("Range of max_tokens should be [1, 65536]") {
		t.Fatal("dashscope range is an output cap")
	}
}

func TestIsOutputCapErrorAnthropicAvailableTokensIsOutputCap(t *testing.T) {
	if !IsOutputCapError("max_tokens: 32768 > context_window: 200000 - input_tokens: 190000 = available_tokens: 10000") {
		t.Fatal("anthropic available tokens is an output cap")
	}
}

func TestIsOutputCapErrorAnthropicMaxTokensOutputCeilingIsOutputCap(t *testing.T) {
	if !IsOutputCapError("max_tokens: 100000 > 64000, which is the maximum allowed number of output tokens for claude-sonnet-4-5") {
		t.Fatal("anthropic output ceiling is an output cap")
	}
}

func TestIsOutputCapErrorRealInputOverflowIsNotOutputCap(t *testing.T) {
	if IsOutputCapError("prompt is too long: 250000 tokens > 200000 max_tokens window") {
		t.Fatal("input overflow is not an output cap")
	}
}

func TestIsOutputCapErrorGpt5UnsupportedParamIsNotOutputCap(t *testing.T) {
	if IsOutputCapError("Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.") {
		t.Fatal("unsupported parameter is not an output cap")
	}
}

func TestIsOutputCapErrorUnrelatedErrorIsNotOutputCap(t *testing.T) {
	if IsOutputCapError("some unrelated 400 error") {
		t.Fatal("unrelated error is not an output cap")
	}
}

const vllmOutputCapMsg = "This model's maximum context length is 131072 tokens. However, you " +
	"requested 65536 output tokens and your prompt contains at least " +
	"65537 input tokens, for a total of at least 131073 tokens. Please " +
	"reduce the length of the input prompt or the number of requested " +
	"output tokens."

const vllmOutputCapRealInputMsg = "This model's maximum context length is 131072 tokens. However, you " +
	"requested 65536 output tokens and your prompt contains 100000 " +
	"input tokens, for a total of 165536 tokens. Please reduce the length " +
	"of the input prompt or the number of requested output tokens."

func TestParseVllmTokenBasedOutputCapVllmTokenBasedFormat(t *testing.T) {
	if got := availableOutputTokens(t, vllmOutputCapMsg); got != 32768 {
		t.Fatalf("got %d", got)
	}
}

func TestParseVllmTokenBasedOutputCapVllmMeasuredInputIsTrusted(t *testing.T) {
	if got := availableOutputTokens(t, vllmOutputCapRealInputMsg); got != 31072 {
		t.Fatalf("got %d", got)
	}
}

func TestParseVllmTokenBasedOutputCapVllmRetryConverges(t *testing.T) {
	window, realInput, cap := 102400, 37000, 65536
	for range 5 {
		if realInput+cap <= window {
			break
		}
		msg := fmt.Sprintf("This model's maximum context length is %d tokens. "+
			"However, you requested %d output tokens and your prompt "+
			"contains at least %d input tokens, for a total of at least %d tokens.", window, cap, window+1-cap, window+1)
		available := availableOutputTokens(t, msg)
		if available >= cap {
			t.Fatal("each retry must lower the cap")
		}
		cap = available
	}
	if realInput+cap > window {
		t.Fatalf("did not converge: cap=%d", cap)
	}
}

func TestParseAdvertisedCeilingWordingsCeilingIsParsedAndClassifiedAsOutputCap(t *testing.T) {
	cases := []struct {
		msg       string
		available int
	}{
		{"Error: max_tokens is too large: 65536. This model supports at most 32768 completion tokens.", 32768},
		{"Requested token count exceeds the model's maximum context length of 131072 tokens. You requested " +
			"a total of 132528 tokens: 66992 tokens from the input messages and 65536 tokens for the completion. " +
			"Please reduce the number of tokens in the input messages or the completion to fit within the limit.", 64080},
	}
	for _, c := range cases {
		if got := availableOutputTokens(t, c.msg); got != c.available {
			t.Fatalf("%q -> %d want %d", c.msg, got, c.available)
		}
		if !IsOutputCapError(c.msg) {
			t.Fatalf("%q is an output cap", c.msg)
		}
	}
}

func TestParseAdvertisedCeilingWordingsSglangInputAloneOverWindowRoutesToCompression(t *testing.T) {
	msg := "Requested token count exceeds the model's maximum context length of 100000 tokens. You requested " +
		"a total of 150000 tokens: 120000 tokens from the input messages and 30000 tokens for the completion."
	if _, ok := ParseAvailableOutputTokensFromError(msg); ok {
		t.Fatal("an input over the window parses no output budget")
	}
	if IsOutputCapError(msg) {
		t.Fatal("an input over the window is not an output cap")
	}
}

func TestLimitedToPhrasingIsAnOutputCap(t *testing.T) {
	if !IsOutputCapError("max_completion_tokens is limited to 16384 for glm-5.2") {
		t.Fatal("limited to phrasing is an output cap")
	}
	if got := availableOutputTokens(t, "max_completion_tokens is limited to 16384 for glm-5.2"); got != 16384 {
		t.Fatalf("got %d", got)
	}
	if IsOutputCapError("prompt is too long: max_tokens limited to 100 given the input") {
		t.Fatal("a prompt too long message is not an output cap")
	}
}

func TestParseOpenAiCompletionSplitSplitIsOutputCapWithWindowMinusMeasuredPrompt(t *testing.T) {
	cases := []struct {
		msg    string
		budget int
	}{
		{"This model's maximum context length is 102400 tokens. However, you requested 102401 tokens " +
			"(36865 in the messages, 65536 in the completion). Please reduce the length of the messages or completion.", 102400 - 36865},
		{"This model's maximum context length is 4097 tokens, however you requested 4771 tokens " +
			"(771 in your prompt; 4000 for the completion). Please reduce your prompt; or completion length.", 4097 - 771},
	}
	for _, c := range cases {
		if got := availableOutputTokens(t, c.msg); got != c.budget {
			t.Fatalf("%q -> %d want %d", c.msg, got, c.budget)
		}
		if !IsOutputCapError(c.msg) {
			t.Fatalf("%q is an output cap", c.msg)
		}
	}
}

func TestParseOpenAiCompletionSplitSplitWithPromptFillingWindowStaysOnCompression(t *testing.T) {
	msg := "This model's maximum context length is 4097 tokens. However, you requested 6000 tokens " +
		"(5000 in the messages, 1000 in the completion). Please reduce the length of the messages or completion."
	if _, ok := ParseAvailableOutputTokensFromError(msg); ok {
		t.Fatal("a prompt filling the window parses no output budget")
	}
	if IsOutputCapError(msg) {
		t.Fatal("a prompt filling the window is not an output cap")
	}
}
