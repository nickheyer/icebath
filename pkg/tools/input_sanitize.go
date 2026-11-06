package tools

import (
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// Degraded visible bracketed paste forms, matched only at boundaries so embedded literals stay intact
var inputBoundarySubs = []struct {
	re   *regexp2.Regexp
	repl string
}{
	{mustPyre(`(^|[\s\n>:\]\)])\[200~`), "$1"},
	{mustPyre(`\[201~(?=$|[\s\n<\[\(\):;.,!?])`), ""},
	{mustPyre(`(^|[\s\n>:\]\)])00~`), "$1"},
	{mustPyre(`01~(?=$|[\s\n<\[\(\):;.,!?])`), ""},
}

// Corruption signature from desktop bracketed paste leaks
const desktopPasteArtifact = "~[[e"

// Removes canonical paste wrappers everywhere and degraded visible forms only at word boundaries
func StripLeakedBracketedPasteWrappers(text string) string {
	if text == "" {
		return text
	}
	for _, wrapper := range []string{"\x1b[200~", "\x1b[201~", "^[[200~", "^[[201~"} {
		text = strings.ReplaceAll(text, wrapper, "")
	}
	for _, sub := range inputBoundarySubs {
		if out, err := sub.re.Replace(text, sub.repl, -1, -1); err == nil {
			text = out
		}
	}
	return text
}

// Drops a trailing run of at least minRepeats desktop paste corruption markers and their lead in
func CollapseRepeatedInputArtifacts(text string, minRepeats int) string {
	if text == "" {
		return text
	}
	marker := desktopPasteArtifact
	index := len(text)
	repeatCount := 0
	for index >= len(marker) && text[index-len(marker):index] == marker {
		repeatCount++
		index -= len(marker)
	}
	if repeatCount < minRepeats {
		return text
	}
	if index >= 2 && text[index-2:index] == "[e" {
		index -= 2
	} else if index >= 1 && text[index-1] == '[' {
		index--
	}
	return text[:index]
}

// Normalizes user authored prompt text before persistence or model input
func SanitizeUserPromptText(text string) string {
	if text == "" {
		return text
	}
	return CollapseRepeatedInputArtifacts(StripLeakedBracketedPasteWrappers(text), 4)
}
