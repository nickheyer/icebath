package catalog

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

var contextLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`max_model_len\s*(?:is\s*)?[:=(]?\s*(\d{4,})`),
	regexp.MustCompile(`maximum model length\s*(?:is\s*)?[:=(]?\s*(\d{4,})`),
	regexp.MustCompile(`(?:max(?:imum)?|limit)\s*(?:context\s*)?(?:length|size|window)?\s*(?:is|of|:)?\s*(\d{4,})`),
	regexp.MustCompile(`context\s*(?:length|size|window)\s*(?:is|of|:)?\s*(\d{4,})`),
	regexp.MustCompile(`(\d{4,})\s*(?:token)?\s*(?:context|limit)`),
	regexp.MustCompile(`>\s*(\d{4,})\s*(?:max|limit|token)`),
	regexp.MustCompile(`(\d{4,})\s*(?:max(?:imum)?)\b`),
	regexp.MustCompile(`supports?\s+(?:only\s+)?up\s+to\s+(\d{4,})`),
}

var outputCapPatterns = []*regexp.Regexp{
	regexp.MustCompile(`exceeds model(?:'s)? maximum output tokens\s*\(?\s*(\d+)\s*\)?`),
	regexp.MustCompile(`max_tokens\s*:\s*\d+\s*>\s*(\d+)\s*,?\s*which is the maximum allowed number of output tokens`),
	regexp.MustCompile(`range of max_tokens should be\s*\[\s*\d+\s*,\s*(\d+)\s*\]`),
	regexp.MustCompile(`available_tokens[:\s]+(\d+)`),
	regexp.MustCompile(`available\s+tokens[:\s]+(\d+)`),
	regexp.MustCompile(`output limit (?:of|is)\s*(\d+)`),
	regexp.MustCompile(`supports at most\s+(\d+)\s*(?:completion\s+)?tokens`),
	regexp.MustCompile(`(?:max_tokens|max_completion_tokens) is limited to\s*(\d+)`),
	regexp.MustCompile(`=\s*(\d+)\s*$`),
}

var (
	completionSplit   = regexp.MustCompile(`\((\d+)\s+(?:tokens\s+)?in (?:the messages|your prompt|the prompt)\s*[;,]\s*(\d+)\s+(?:tokens\s+)?(?:in|for) the completion\)`)
	maxContextIs      = regexp.MustCompile(`maximum context length is (\d+)`)
	maxContextIsToken = regexp.MustCompile(`maximum context length is (\d+)\s*token`)
	maxContextOfToken = regexp.MustCompile(`maximum context length of (\d+)\s*token`)
	textToolOutput    = regexp.MustCompile(`\((\d+)\s+of text input,\s*(\d+)\s+of tool input,\s*(\d+)\s+in the output\)`)
	promptCharacters  = regexp.MustCompile(`prompt contains (\d+)\s*character`)
	promptInputTokens = regexp.MustCompile(`prompt contains (?:at least )?(\d+)\s*input tokens`)
	requestedOutput   = regexp.MustCompile(`requested (\d+)\s*output tokens`)
	inputMessageCount = regexp.MustCompile(`(\d+)\s*tokens from the input messages`)
)

var outputCapSignals = [][]string{
	{"range of max_tokens should be"}, {"available_tokens"}, {"available tokens"},
	{"in the output", "maximum context length"}, {"requested", "output tokens"},
	{"should be"}, {"less than or equal"}, {"must be"}, {"exceeds model", "maximum output tokens"},
	{"output limit"}, {"maximum allowed number of output tokens"},
	{"max_tokens is too large", "supports at most"}, {"tokens from the input messages", "tokens for the completion"},
	{"limited to"},
}

var inputOverflowSignals = []string{
	"prompt is too long", "prompt too long", "input is too long", "input token",
	"prompt length", "prompt contains", "reduce the length",
}

var parseableOutputCapSignals = [][]string{
	{"max_tokens", "available_tokens"}, {"max_tokens", "available tokens"},
	{"in the output", "maximum context length"},
	{"maximum context length", "requested", "output tokens"},
	{"maximum context length", "in the completion"}, {"maximum context length", "for the completion"},
	{"range of max_tokens should be"}, {"exceeds model", "maximum output tokens"},
	{"output limit"}, {"max_tokens", "maximum allowed number of output tokens"},
	{"max_tokens is too large", "supports at most"}, {"tokens from the input messages", "tokens for the completion"},
	{"limited to"},
}

var outputCapWords = []string{"max_tokens", "max_output_tokens", "max_completion_tokens", "tokens for the completion"}

func digits(text string) int {
	n, err := strconv.Atoi(text)
	if err != nil {
		return math.MaxInt32
	}
	return n
}

func firstGroup(re *regexp.Regexp, text string) (int, bool) {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	return digits(m[1]), true
}

func anyPhraseGroup(text string, groups [][]string) bool {
	for _, group := range groups {
		all := true
		for _, phrase := range group {
			if !strings.Contains(text, phrase) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func containsAny(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func ParseContextLimitFromError(message string) (int, bool) {
	lower := strings.ToLower(message)
	if containsAny(lower, []string{"output limit", "output tokens", "output token"}) && !strings.Contains(lower, "context") {
		return 0, false
	}
	for _, pattern := range contextLimitPatterns {
		limit, ok := firstGroup(pattern, lower)
		if ok && limit >= 1024 && limit <= 10_000_000 {
			return limit, true
		}
	}
	return 0, false
}

func ContextLengthFromProviderError(message string, current int) (int, bool) {
	limit, ok := ParseContextLimitFromError(message)
	if !ok || limit >= current {
		return 0, false
	}
	return limit, true
}

func completionSplitBudget(lower string) (int, bool) {
	split := completionSplit.FindStringSubmatch(lower)
	window, ok := firstGroup(maxContextIs, lower)
	if split == nil || !ok {
		return 0, false
	}
	available := window - digits(split[1])
	return available, available >= 1
}

func sglangWindowAndInput(lower string) (window, input int, ok bool) {
	window, okWindow := firstGroup(maxContextOfToken, lower)
	input, okInput := firstGroup(inputMessageCount, lower)
	return window, input, okWindow && okInput
}

func ParseAvailableOutputTokensFromError(message string) (int, bool) {
	lower := strings.ToLower(message)
	if !anyPhraseGroup(lower, parseableOutputCapSignals) {
		return 0, false
	}
	for _, pattern := range outputCapPatterns {
		if n, ok := firstGroup(pattern, lower); ok && n >= 1 {
			return n, true
		}
	}
	window, hasWindow := firstGroup(maxContextIs, lower)
	if parts := textToolOutput.FindStringSubmatch(lower); hasWindow && parts != nil {
		if available := window - digits(parts[1]) - digits(parts[2]); available >= 1 {
			return available, true
		}
	}
	if available, ok := completionSplitBudget(lower); ok {
		return available, true
	}
	tokenWindow, hasTokenWindow := firstGroup(maxContextIsToken, lower)
	if chars, ok := firstGroup(promptCharacters, lower); hasTokenWindow && ok {
		if available := tokenWindow - (chars+2)/3; available >= 1 {
			return available, true
		}
	}
	if sgWindow, sgInput, ok := sglangWindowAndInput(lower); ok && sgWindow-sgInput >= 1 {
		return sgWindow - sgInput, true
	}
	if input, ok := firstGroup(promptInputTokens, lower); hasTokenWindow && ok {
		available := tokenWindow - input
		if requested, ok := firstGroup(requestedOutput, lower); ok && strings.Contains(lower, "at least") {
			if available >= requested-1 {
				return max(1, requested/2), true
			}
		}
		if available >= 1 {
			return available, true
		}
	}
	return 0, false
}

func IsOutputCapError(message string) bool {
	lower := strings.ToLower(message)
	if _, ok := completionSplitBudget(lower); ok {
		return true
	}
	sgWindow, sgInput, sglang := sglangWindowAndInput(lower)
	return containsAny(lower, outputCapWords) &&
		anyPhraseGroup(lower, outputCapSignals) &&
		!containsAny(lower, inputOverflowSignals) &&
		!(sglang && sgInput >= sgWindow)
}
