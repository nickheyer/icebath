package tui

import (
	"regexp"
	"strings"
)

// Tags a reasoning model wraps its scratchpad in, checked in this order
var reasoningTags = []string{"think", "reasoning", "thinking", "thought", "REASONING_SCRATCHPAD"}

type reasoningPattern struct {
	tag      string
	paired   *regexp.Regexp
	unclosed *regexp.Regexp
}

var reasoningPatterns = func() []reasoningPattern {
	out := make([]reasoningPattern, len(reasoningTags))
	for i, tag := range reasoningTags {
		out[i] = reasoningPattern{
			tag:      tag,
			paired:   regexp.MustCompile(`(?i)<` + tag + `>([\s\S]*?)</` + tag + `>[` + jsWS + `]*`),
			unclosed: regexp.MustCompile(`(?i)^[` + jsWS + `]*<` + tag + `>([\s\S]*)$`),
		}
	}
	return out
}()

// Reasoning and visible text of a reply that carries reasoning tags
type splitReply struct {
	Reasoning string
	Text      string
}

// Separates tagged reasoning from the reply, a leading unclosed tag taking the rest as reasoning
func splitReasoning(input string) splitReply {
	text := input
	var reasoning []string
	take := func(re *regexp.Regexp) {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			if inner := strings.TrimFunc(re.FindStringSubmatch(m)[1], isJSSpace); inner != "" {
				reasoning = append(reasoning, inner)
			}
			return ""
		})
	}
	for _, p := range reasoningPatterns {
		take(p.paired)
		take(p.unclosed)
	}
	return splitReply{
		Reasoning: strings.TrimFunc(strings.Join(reasoning, "\n\n"), isJSSpace),
		Text:      strings.TrimFunc(text, isJSSpace),
	}
}

// Whether text opens any reasoning tag, matched case sensitively
func hasReasoningTag(input string) bool {
	for _, tag := range reasoningTags {
		if strings.Contains(input, "<"+tag+">") {
			return true
		}
	}
	return false
}
