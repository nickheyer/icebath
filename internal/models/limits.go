package models

import (
	"regexp"
	"strconv"
	"strings"
)

// Ways a provider's refusal of a too long request states the window
var windowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`max_model_len\s*(?:is\s*)?[:=(]?\s*(\d{4,})`),
	regexp.MustCompile(`maximum model length\s*(?:is\s*)?[:=(]?\s*(\d{4,})`),
	regexp.MustCompile(`(?:max(?:imum)?|limit)\s*(?:context\s*)?(?:length|size|window)?\s*(?:is|of|:)?\s*(\d{4,})`),
	regexp.MustCompile(`context\s*(?:length|size|window)\s*(?:is|of|:)?\s*(\d{4,})`),
	regexp.MustCompile(`(\d{4,})\s*(?:token)?\s*(?:context|limit)`),
	regexp.MustCompile(`>\s*(\d{4,})\s*(?:max|limit|token)`),
	regexp.MustCompile(`(\d{4,})\s*(?:max(?:imum)?)\b`),
	regexp.MustCompile(`supports?\s+(?:only\s+)?up\s+to\s+(\d{4,})`),
}

// Context window a provider's error quotes, none for an error about output alone
func ContextLimit(message string) (int, bool) {
	lower := strings.ToLower(message)
	if (strings.Contains(lower, "output limit") || strings.Contains(lower, "output token")) && !strings.Contains(lower, "context") {
		return 0, false
	}
	for _, pattern := range windowPatterns {
		if match := pattern.FindStringSubmatch(lower); match != nil {
			if n, err := strconv.Atoi(match[1]); err == nil && n >= 1024 && n <= 10_000_000 {
				return n, true
			}
		}
	}
	return 0, false
}

// Window a provider's error quotes when it is below the one assumed, which then was too big
func LowerContextLimit(message string, current int) (int, bool) {
	n, ok := ContextLimit(message)
	return n, ok && n < current
}

var (
	completionSplit = regexp.MustCompile(`\((\d+)\s+(?:tokens\s+)?in (?:the messages|your prompt|the prompt)\s*[;,]\s*(\d+)\s+(?:tokens\s+)?(?:in|for) the completion\)`)
	maximumIs       = regexp.MustCompile(`maximum context length is (\d+)`)
	maximumTokens   = regexp.MustCompile(`maximum context length is (\d+)\s*token`)
	outputParts     = regexp.MustCompile(`\((\d+)\s+of text input,\s*(\d+)\s+of tool input,\s*(\d+)\s+in the output\)`)
	promptChars     = regexp.MustCompile(`prompt contains (\d+)\s*character`)
	vllmInput       = regexp.MustCompile(`prompt contains (?:at least )?(\d+)\s*input tokens`)
	requestedOutput = regexp.MustCompile(`requested (\d+)\s*output tokens`)
	sglangWindow    = regexp.MustCompile(`maximum context length of (\d+)\s*token`)
	sglangInput     = regexp.MustCompile(`(\d+)\s*tokens from the input messages`)
)

// Ways a refusal of too many output tokens states how many there may be
var outputPatterns = []*regexp.Regexp{
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

// Phrases that together say a refusal is of the output's size, and those of a refusal that can be read for how much output there may be
var (
	outputSignals = [][]string{
		{"range of max_tokens should be"}, {"available_tokens"}, {"available tokens"}, {"in the output", "maximum context length"},
		{"requested", "output tokens"}, {"should be"}, {"less than or equal"}, {"must be"}, {"exceeds model", "maximum output tokens"},
		{"output limit"}, {"maximum allowed number of output tokens"}, {"max_tokens is too large", "supports at most"},
		{"tokens from the input messages", "tokens for the completion"}, {"limited to"},
	}
	readableSignals = [][]string{
		{"max_tokens", "available_tokens"}, {"max_tokens", "available tokens"}, {"in the output", "maximum context length"},
		{"maximum context length", "requested", "output tokens"}, {"maximum context length", "in the completion"},
		{"maximum context length", "for the completion"}, {"range of max_tokens should be"}, {"exceeds model", "maximum output tokens"},
		{"output limit"}, {"max_tokens", "maximum allowed number of output tokens"}, {"max_tokens is too large", "supports at most"},
		{"tokens from the input messages", "tokens for the completion"}, {"limited to"},
	}
	inputSignals = []string{"prompt is too long", "prompt too long", "input is too long", "input token", "prompt length", "prompt contains", "reduce the length"}
)

func anyGroup(text string, groups [][]string) bool {
	for _, group := range groups {
		all := true
		for _, phrase := range group {
			all = all && strings.Contains(text, phrase)
		}
		if all {
			return true
		}
	}
	return false
}

// Window less the prompt, from OpenAI's split of a request between the messages and the completion
func splitBudget(lower string) (int, bool) {
	split, window := completionSplit.FindStringSubmatch(lower), maximumIs.FindStringSubmatch(lower)
	if split == nil || window == nil {
		return 0, false
	}
	total, _ := strconv.Atoi(window[1])
	prompt, _ := strconv.Atoi(split[1])
	return total - prompt, total-prompt >= 1
}

// Window and input of SGLang's wording
func sglang(lower string) (int, int, bool) {
	window, input := sglangWindow.FindStringSubmatch(lower), sglangInput.FindStringSubmatch(lower)
	if window == nil || input == nil {
		return 0, 0, false
	}
	w, _ := strconv.Atoi(window[1])
	i, _ := strconv.Atoi(input[1])
	return w, i, true
}

// Output tokens a refusal of too many says there may be
func OutputBudget(message string) (int, bool) {
	lower := strings.ToLower(message)
	if !anyGroup(lower, readableSignals) {
		return 0, false
	}
	for _, pattern := range outputPatterns {
		if match := pattern.FindStringSubmatch(lower); match != nil {
			if n, err := strconv.Atoi(match[1]); err == nil && n >= 1 {
				return n, true
			}
		}
	}
	if window, parts := maximumIs.FindStringSubmatch(lower), outputParts.FindStringSubmatch(lower); window != nil && parts != nil {
		total, _ := strconv.Atoi(window[1])
		text, _ := strconv.Atoi(parts[1])
		tool, _ := strconv.Atoi(parts[2])
		if left := total - text - tool; left >= 1 {
			return left, true
		}
	}
	if n, ok := splitBudget(lower); ok {
		return n, true
	}
	window := maximumTokens.FindStringSubmatch(lower)
	if chars := promptChars.FindStringSubmatch(lower); window != nil && chars != nil {
		total, _ := strconv.Atoi(window[1])
		c, _ := strconv.Atoi(chars[1])
		if left := total - (c+2)/3; left >= 1 {
			return left, true
		}
	}
	if total, input, ok := sglang(lower); ok && total-input >= 1 {
		return total - input, true
	}
	if input := vllmInput.FindStringSubmatch(lower); window != nil && input != nil {
		total, _ := strconv.Atoi(window[1])
		used, _ := strconv.Atoi(input[1])
		left := total - used
		if requested := requestedOutput.FindStringSubmatch(lower); strings.Contains(lower, "at least") && requested != nil {
			asked, _ := strconv.Atoi(requested[1])
			if left >= asked-1 {
				return max(asked/2, 1), true
			}
		}
		if left >= 1 {
			return left, true
		}
	}
	return 0, false
}

// Whether a refusal is of the output asked for being too big, not of the input
func OutputCapError(message string) bool {
	lower := strings.ToLower(message)
	if _, ok := splitBudget(lower); ok {
		return true
	}
	total, input, isSGLang := sglang(lower)
	capped := false
	for _, phrase := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens", "tokens for the completion"} {
		capped = capped || strings.Contains(lower, phrase)
	}
	if !capped || !anyGroup(lower, outputSignals) {
		return false
	}
	for _, phrase := range inputSignals {
		if strings.Contains(lower, phrase) {
			return false
		}
	}
	return !(isSGLang && input >= total)
}
