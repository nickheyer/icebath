package agent

import (
	"regexp"
	"strings"
	"unicode"

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

var trailingContinueIntentRe = regexp.MustCompile(`(?i)(?:\blet me now\b|\bi(?:['\x{2019}])?ll now\b|\bi will now\b` +
	`|\bnow i(?:['\x{2019}]ll| will)\b|\bnext[,:] i\b)` +
	`[^.!?\n]{0,100}[.:\x{2026}]?\s*$`)

var promotedReasoningPlanTailRe = regexp.MustCompile(`(?i)(?:^|[.!?:\x{3002}\x{ff01}\x{ff1f}\x{2014}\x{2013}\n]\s*|\x{2026}\s*)` +
	`(?:let(?:['\x{2019}]s| me)\b|i(?:['\x{2019}]ll| will| need to| should| am going to|['\x{2019}]m going to)\b` +
	`|next[,:]? i\b|now i(?:['\x{2019}]ll| will| need to)\b|first[,:]? i(?:['\x{2019}]ll| will| need to)\b` +
	`|\x{0e08}\x{0e30}\x{0e43}\x{0e2b}\x{0e49}\x{0e1c}\x{0e21}|\x{0e1c}\x{0e21}\x{0e08}\x{0e30}` +
	`|\x{0e15}\x{0e48}\x{0e2d}\x{0e44}\x{0e1b}(?:\x{0e08}\x{0e30}|\x{0e1c}\x{0e21}\x{0e08}\x{0e30})` +
	`|\x{0e02}\x{0e2d}(?:\x{0e40}\x{0e23}\x{0e34}\x{0e48}\x{0e21}|\x{0e25}\x{0e2d}\x{0e07}|\x{0e15}\x{0e23}\x{0e27}\x{0e08}|\x{0e41}\x{0e01}\x{0e49}|\x{0e2a}\x{0e48}\x{0e07}|\x{0e17}\x{0e33}|\x{0e14}\x{0e39})` +
	`|\x{0e08}\x{0e30}(?:\x{0e40}\x{0e23}\x{0e34}\x{0e48}\x{0e21}|\x{0e25}\x{0e2d}\x{0e07}|\x{0e15}\x{0e23}\x{0e27}\x{0e08}|\x{0e41}\x{0e01}\x{0e49}|\x{0e2a}\x{0e48}\x{0e07}|\x{0e17}\x{0e33}|\x{0e14}\x{0e39}|\x{0e23}\x{0e31}\x{0e19}|\x{0e22}\x{0e34}\x{0e07}))` +
	`[^.!?\n\x{3002}\x{ff01}\x{ff1f}]{0,160}(?:[.:\x{2026}]+)?\s*$`)

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
	return trailingContinueIntentRe.MatchString(lastRunes(t, 160))
}

// Whether promoted reasoning ends on a first person plan to act
func promotedReasoningAnnouncesAction(text string) bool {
	t := pystr.Strip(text)
	if t == "" {
		return false
	}
	return promotedReasoningPlanTailRe.MatchString(lastRunes(t, 240))
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
