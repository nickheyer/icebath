package commands

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Reports a shell mode submission, a string whose stripped text starts with an exclamation mark
func IsBangCommand(text any) bool {
	s, ok := text.(string)
	return ok && strings.HasPrefix(pystr.Strip(s), "!")
}

// Shell command inside a bang submission with exactly one leading mark removed, empty when bare
func ParseBangCommand(text string) string {
	if !IsBangCommand(text) {
		return ""
	}
	return pystr.Strip(pystr.Strip(text)[1:])
}
