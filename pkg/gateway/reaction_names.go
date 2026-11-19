package gateway

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// Reaction name of an emoji for platforms that react by name, a bare or colon wrapped name kept as given
func ReactionShortcode(emoji string) (string, bool) {
	trimmed := strings.Trim(strings.TrimSpace(emoji), ":")
	if trimmed != "" && strings.Trim(trimmed, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-+") == "" {
		return strings.ToLower(trimmed), true
	}
	return pyrich.EmojiShortName(emoji)
}
