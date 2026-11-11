package gateway

import (
	"regexp"
	"strings"
)

var (
	completeFencedRegionRe = regexp.MustCompile("(?s)```.*?```")
	trailingOpenFenceRe    = regexp.MustCompile("```[^`]*$")
)

// Text with each triple backtick escaped so it can sit inside an outer code block
func EscapeCodeFencesForDisplay(text string) string {
	return strings.ReplaceAll(text, "```", "\\`\\`\\`")
}

// Text with an orphaned code fence or inline backtick closed so a truncated reply renders sanely
func EnsureClosedCodeFences(text string) string {
	if text == "" {
		return text
	}
	if strings.Count(text, "```")%2 == 1 {
		text = strings.TrimRight(text, "\n") + "\n```"
	}
	withoutFences := completeFencedRegionRe.ReplaceAllString(text, "")
	withoutFences = trailingOpenFenceRe.ReplaceAllString(withoutFences, "")
	if strings.Count(withoutFences, "`")%2 == 1 {
		text += "`"
	}
	return text
}
