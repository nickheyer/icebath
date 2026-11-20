package agent

import (
	"strings"
	"unicode"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	degenerateFinalMaxChars        = 24
	degenerateLeadingPunct         = "?!,;:)]}"
	trailingContinueIntentMaxChars = 400
)

var sentenceTerminals = []string{".", "!", "?", "。", "！", "？"}

var trailingContinueIntentRe = mustPyIgnoreCase(`(?:\blet me now\b|\bi(?:['\u2019])?ll now\b|\bi will now\b` +
	`|\bnow i(?:['\u2019]ll| will)\b|\bnext[,:] i\b)` +
	`[^.!?\n]{0,100}[.:\u2026]?\s*$`)

var promotedReasoningPlanTailRe = mustPyIgnoreCase(`(?:^|[.!?:\u3002\uff01\uff1f\u2014\u2013\n]\s*|\u2026\s*)` +
	`(?:let(?:['\u2019]s| me)\b|i(?:['\u2019]ll| will| need to| should| am going to|['\u2019]m going to)\b` +
	`|next[,:]? i\b|now i(?:['\u2019]ll| will| need to)\b|first[,:]? i(?:['\u2019]ll| will| need to)\b` +
	`|\u0e08\u0e30\u0e43\u0e2b\u0e49\u0e1c\u0e21|\u0e1c\u0e21\u0e08\u0e30` +
	`|\u0e15\u0e48\u0e2d\u0e44\u0e1b(?:\u0e08\u0e30|\u0e1c\u0e21\u0e08\u0e30)` +
	`|\u0e02\u0e2d(?:\u0e40\u0e23\u0e34\u0e48\u0e21|\u0e25\u0e2d\u0e07|\u0e15\u0e23\u0e27\u0e08|\u0e41\u0e01\u0e49|\u0e2a\u0e48\u0e07|\u0e17\u0e33|\u0e14\u0e39)` +
	`|\u0e08\u0e30(?:\u0e40\u0e23\u0e34\u0e48\u0e21|\u0e25\u0e2d\u0e07|\u0e15\u0e23\u0e27\u0e08|\u0e41\u0e01\u0e49|\u0e2a\u0e48\u0e07|\u0e17\u0e33|\u0e14\u0e39|\u0e23\u0e31\u0e19|\u0e22\u0e34\u0e07))` +
	`[^.!?\n\u3002\uff01\uff1f]{0,160}(?:[.:\u2026]+)?\s*$`)

// Python re.IGNORECASE pattern with Python matching semantics, panicking on a bad pattern
func mustPyIgnoreCase(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileIgnoreCase(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

var (
	intentAckOn  = map[string]bool{"true": true, "always": true, "yes": true, "on": true}
	intentAckOff = map[string]bool{"false": true, "never": true, "no": true, "off": true}
)

// Whether a text stop reads as a collapsed fragment rather than a terse answer
func looksLikeDegenerateFinal(text string, userMessage any) bool {
	t := pystr.Strip(text)
	r := []rune(t)
	if len(r) == 0 || len(r) > degenerateFinalMaxChars {
		return false
	}
	for _, term := range sentenceTerminals {
		if strings.HasSuffix(t, term) {
			return false
		}
	}
	if strings.ContainsRune(degenerateLeadingPunct, r[0]) && len(r) > 1 && pystr.IsAlpha(r[1]) {
		return true
	}
	anyAlpha, asciiAlnum := false, false
	for _, ch := range r {
		anyAlpha = anyAlpha || pystr.IsAlpha(ch)
		asciiAlnum = asciiAlnum || (ch <= unicode.MaxASCII && pystr.IsAlnum(ch))
	}
	if !anyAlpha || asciiAlnum {
		return false
	}
	userText := ""
	if pyTruthy(userMessage) {
		userText = provider.SummarizeUserMessageForLog(userMessage, " ")
	}
	for _, ch := range userText {
		if pystr.IsAlpha(ch) && ch > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// Tool result rows after the most recent user row
func toolResultsThisTurn(messages []*state.Message) int {
	count := 0
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m == nil {
			continue
		}
		if m.Role == "user" {
			break
		}
		if m.Role == "tool" {
			count++
		}
	}
	return count
}

// Whether a short reply ends on an announced next action
func trailingContinueIntent(text string) bool {
	t := pystr.Strip(text)
	if n := len([]rune(t)); n == 0 || n > trailingContinueIntentMaxChars {
		return false
	}
	return pyReSearch(trailingContinueIntentRe, lastRunes(t, 160))
}

// Whether promoted reasoning ends on a first person plan to act
func promotedReasoningAnnouncesAction(text string) bool {
	t := pystr.Strip(text)
	if t == "" {
		return false
	}
	return pyReSearch(promotedReasoningPlanTailRe, lastRunes(t, 240))
}

// Intent ack continuation mode off, codex_only or all from the agent setting and live route
func intentAckContinuationMode(a *AIAgent) string {
	mode := a.intentAckContinuation
	if b, ok := mode.(bool); ok {
		if b {
			return "all"
		}
		return "off"
	}
	if s, ok := mode.(string); ok {
		if intentAckOn[strings.ToLower(s)] {
			return "all"
		}
		if intentAckOff[strings.ToLower(s)] {
			return "off"
		}
	}
	if list, ok := mode.([]any); ok {
		modelLower := strings.ToLower(a.model)
		for _, p := range list {
			if s, ok := p.(string); ok && strings.Contains(modelLower, strings.ToLower(s)) {
				return "all"
			}
		}
		return "off"
	}
	if a.apiMode == "codex_responses" {
		return "codex_only"
	}
	return "off"
}
