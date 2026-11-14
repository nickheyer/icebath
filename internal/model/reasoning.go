package model

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

const reasoningMandatoryPattern = "reasoning is mandatory"

var unsupportedParamMarkers = []string{
	"unsupported parameter", "unsupported_parameter", "not supported", "does not support",
	"doesn't support", "is deprecated for this model",
	"unknown parameter", "unrecognized request argument", "unrecognized parameter",
	"invalid parameter", "extra inputs are not permitted",
	"invalid option: expected one of",
}

var reasoningFieldTokens = []string{
	"reasoning_effort", "thinking_config", "thinking_budget", "enable_thinking", "thinkingconfig",
	"thinkingbudget", "reasoning", "thinking", "think",
}

var reasoningParamRejection = regexp.MustCompile(
	`['"]param['"]\s*:\s*['"](?:reasoning(?:[._]effort)?|thinking(?:_config|_budget)?|enable_thinking)['"]` +
		`|invalid_reasoning_effort`)

var reasoningRequiredMarkers = []string{
	"mandatory", "cannot be disabled", "can't be disabled", "must be enabled", "is required",
	"always enabled", "cannot be turned off",
}

var reasoningLevelRejectionMarkers = []string{"must be one of", "invalid value for"}

func wordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func tokenBoundary(r rune) bool {
	return wordRune(r) || r == '-' || r == '/'
}

func followedByModelWord(rest []rune) bool {
	i := 0
	for i < len(rest) && unicode.IsSpace(rest[i]) {
		i++
	}
	if i == 0 || !strings.HasPrefix(string(rest[i:min(len(rest), i+6)]), "model") {
		return false
	}
	after := rest[i+5:]
	if len(after) == 0 || !wordRune(after[0]) {
		return true
	}
	return after[0] == 's' && (len(after) == 1 || !wordRune(after[1]))
}

func reasoningFieldToken(msg []rune) (start, end int, found bool) {
	for i := range msg {
		if i > 0 && tokenBoundary(msg[i-1]) {
			continue
		}
		for _, token := range reasoningFieldTokens {
			t := []rune(token)
			if len(msg)-i < len(t) || !slices.Equal(msg[i:i+len(t)], t) {
				continue
			}
			j := i + len(t)
			if j < len(msg) && tokenBoundary(msg[j]) {
				continue
			}
			if followedByModelWord(msg[j:]) {
				continue
			}
			return i, j, true
		}
	}
	return 0, 0, false
}

func nearToken(msg []rune, before, after int) (string, bool) {
	start, end, found := reasoningFieldToken(msg)
	if !found {
		return "", false
	}
	return string(msg[max(0, start-before):min(len(msg), end+after)]), true
}

func ReasoningLevelRejection(text string) bool {
	near, ok := nearToken([]rune(strings.ToLower(text)), 48, 96)
	return ok && containsAny(near, reasoningLevelRejectionMarkers)
}

func ReasoningRequiredRejection(text string) bool {
	msg := strings.ToLower(text)
	near, ok := nearToken([]rune(msg), 48, 96)
	if !ok {
		return false
	}
	return containsAny(near, reasoningRequiredMarkers) || ReasoningLevelRejection(msg)
}

func ReasoningFieldRejection(text string) bool {
	msg := strings.ToLower(text)
	if reasoningParamRejection.MatchString(msg) {
		return true
	}
	near, ok := nearToken([]rune(msg), 32, 32)
	if !ok {
		return false
	}
	return strings.Contains(near, "unsupported") || containsAny(msg, unsupportedParamMarkers)
}

func UnsupportedParamError(text, param string) bool {
	param = strings.ToLower(param)
	if param == "" {
		return false
	}
	msg := strings.ToLower(text)
	return strings.Contains(msg, param) && containsAny(msg, unsupportedParamMarkers)
}

type staleFloor struct {
	slug  string
	floor time.Duration
}

var reasoningStaleFloors = sortedFloors(map[time.Duration][]string{
	600 * time.Second: {
		"nemotron-3-ultra", "nemotron-3-super", "nemotron-3.5-super-vl",
		"deepseek-r1", "deepseek-reasoner", "deepseek-flash", "deepseek-v4-flash", "deepseek-v4.1-flash", "deepseek-v4-pro",
		"o1", "o1-mini", "o1-pro", "o1-preview", "o3", "o3-pro",
		"gpt-5.6", "gpt-6",
		"claude-fable",
		"gemini-4",
	},
	300 * time.Second: {
		"nemotron-3-nano", "nemotron-3.5-lightning", "qwq-32b", "o3-mini", "o4-mini",
		"grok-4-fast-reasoning", "grok-4.20-reasoning", "grok-4.5", "grok-4.6",
		"ox-alpha", "x-preview-f-free", "inkling",
		"minimax-m2",
	},
	240 * time.Second: {"claude-opus-4", "claude-opus-5"},
	180 * time.Second: {"qwen3", "claude-sonnet-5", "claude-sonnet-4.5", "claude-sonnet-4.6", "grok-4-fast-non-reasoning"},
})

func sortedFloors(table map[time.Duration][]string) []staleFloor {
	var out []staleFloor
	for floor, slugs := range table {
		for _, slug := range slugs {
			out = append(out, staleFloor{slug, floor})
		}
	}
	slices.SortFunc(out, func(a, b staleFloor) int {
		if len(a.slug) != len(b.slug) {
			return len(b.slug) - len(a.slug)
		}
		return strings.Compare(a.slug, b.slug)
	})
	return out
}

func ReasoningStaleFloor(model string) (time.Duration, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return 0, false
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for _, f := range reasoningStaleFloors {
		if !strings.HasPrefix(name, f.slug) {
			continue
		}
		if len(name) == len(f.slug) || strings.ContainsRune("-._:", rune(name[len(f.slug)])) {
			return f.floor, true
		}
	}
	return 0, false
}
