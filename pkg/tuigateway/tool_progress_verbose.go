package tuigateway

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Verbose tool text budget shipped to the TUI trail line, a hair over its persisted preview
const (
	tuiVerboseTextMaxChars = 1000
	tuiVerboseTextMaxLines = 16
)

// Tail of verbose text within the char and line budget, never cut mid line, with an omitted label
func capTUIVerboseText(text string) string {
	runes := []rune(text)
	if len(runes) <= tuiVerboseTextMaxChars && strings.Count(text, "\n") < tuiVerboseTextMaxLines {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := lines[max(0, len(lines)-tuiVerboseTextMaxLines):]
	lineStart := len(runes) - utf8.RuneCountInString(strings.Join(kept, "\n"))
	start := max(lineStart, len(runes)-tuiVerboseTextMaxChars)
	if start > lineStart {
		next := -1
		for i := start; i < len(runes); i++ {
			if runes[i] == '\n' {
				next = i
				break
			}
		}
		if next >= 0 && next < len(runes)-1 {
			start = next + 1
		}
	}
	tail := pystr.Lstrip(string(runes[start:]))
	omittedChars := max(0, len(runes)-utf8.RuneCountInString(tail))
	omittedLines := strings.Count(string(runes[:start]), "\n")
	omitted := fmt.Sprintf("%d chars", omittedChars)
	if omittedLines > 0 {
		omitted = fmt.Sprintf("%d lines / %d chars", omittedLines, omittedChars)
	}
	return fmt.Sprintf("[showing verbose tail; omitted %s]\n%s", omitted, tail)
}

// Forced redaction of verbose text, then capped to the TUI budget
func redactTUIVerboseText(text string) string {
	return capTUIVerboseText(tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true}))
}

// Redacted and capped plain text view of a tool result for the verbose trail
func ToolResultText(result any) string {
	return redactTUIVerboseText(tools.MultimodalTextSummary(result))
}
