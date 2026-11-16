package app

import (
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Text after the slash command word with its padding removed, empty when there is none
func slashArgs(cmd string) string {
	if parts := pystr.SplitWhitespace(cmd, 1); len(parts) > 1 {
		return pystr.Strip(parts[1])
	}
	return ""
}
