package commands

import (
	"strings"
)

// Reports a shell mode submission, a string whose stripped text starts with an exclamation mark
func IsBangCommand(text any) bool {
	s, ok := text.(string)
	return ok && strings.HasPrefix(strings.TrimSpace(s), "!")
}

// Shell command inside a bang submission with exactly one leading mark removed, empty when bare
func ParseBangCommand(text string) string {
	if !IsBangCommand(text) {
		return ""
	}
	return strings.TrimSpace(strings.TrimSpace(text)[1:])
}
