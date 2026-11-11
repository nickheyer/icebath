package gatewayrun

import (
	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
)

// Slash command mention not preceded by a word character, colon or slash
var telegramCommandMentionRe = pyre.MustCompile(`(?<![\w:/])/([A-Za-z0-9][A-Za-z0-9_-]*)`)

// Lowercased hyphen folded command name Telegram accepts, assigned in cmd/icebath wiring
var SanitizeTelegramName func(raw string) string

// Slash command mentions rewritten to Telegram valid names on Telegram, text unchanged elsewhere
func telegramizeCommandMentions(text string, platform gateway.Platform) string {
	if platform != gateway.PlatformTelegram {
		return text
	}
	out, err := telegramCommandMentionRe.ReplaceFunc(text, func(m regexp2.Match) string {
		if sanitized := SanitizeTelegramName(m.GroupByNumber(1).String()); sanitized != "" {
			return "/" + sanitized
		}
		return m.String()
	}, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}
