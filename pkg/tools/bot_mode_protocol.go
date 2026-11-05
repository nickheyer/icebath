package tools

import (
	"regexp"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// BotModeProtocolHeading opens the Bot Chat teammate section and the plugin era SOUL append
const BotModeProtocolHeading = "## Messaging other agents"

var legacyBotProtocolRe = regexp2.MustCompile(`\n*`+regexp.QuoteMeta(BotModeProtocolHeading)+`[ \t]*\n.*?(?=\n## |\z)`, regexp2.Singleline)

// StripLegacyBotProtocol drops every plugin era teammate section from SOUL text, idempotent
func StripLegacyBotProtocol(text string) string {
	if !strings.Contains(text, BotModeProtocolHeading) {
		return text
	}
	out, err := legacyBotProtocolRe.Replace(text, "\n", -1, -1)
	if err != nil {
		panic(err)
	}
	return pystr.Strip(out) + "\n"
}

// CoerceCapabilityBool is the image routing strict boolean of a capability override, ok false when unset
func CoerceCapabilityBool(raw any) (bool, bool) {
	return coerceCapabilityBool(raw)
}
