package cron

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Bot Chat pseudo platform token and the display policy platform its warnings follow
const (
	BotChatPlatform       = "bot-chat"
	BotChatPolicyPlatform = "tui"
)

// Fire time routing tokens, all expands to every platform with a configured home chat
var routingTokens = map[string]bool{"all": true}

// Canonical comma separated deliver string, local when empty and lists flattened without blanks
func normalizeDeliverValue(deliver any) string {
	switch v := deliver.(type) {
	case nil:
		return "local"
	case string:
		if v == "" {
			return "local"
		}
		return v
	case *string:
		if v == nil {
			return "local"
		}
		return normalizeDeliverValue(*v)
	case []string:
		items := make([]any, len(v))
		for i, s := range v {
			items[i] = s
		}
		return normalizeDeliverValue(items)
	case []any:
		var parts []string
		for _, p := range v {
			if s := strings.TrimSpace(pyrepr.Str(p)); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return "local"
		}
		return strings.Join(parts, ",")
	}
	return pyrepr.Str(deliver)
}

// Raw deliver lane of a run outcome, the failure lane when it is set for a failure else deliver
func deliveryLaneValue(job *Job, forFailure bool) any {
	if forFailure && job.FailureDeliver != nil && strings.TrimSpace(*job.FailureDeliver) != "" {
		return *job.FailureDeliver
	}
	if job.Deliver == nil {
		return nil
	}
	return *job.Deliver
}

// Profile argument of a bot-chat token, empty for the own profile, false when the part is not one
func parseBotChatDeliverToken(part string) (string, bool) {
	raw := strings.TrimSpace(part)
	lowered := strings.ToLower(raw)
	if lowered == BotChatPlatform {
		return "", true
	}
	prefix := BotChatPlatform + ":"
	if strings.HasPrefix(lowered, prefix) {
		return strings.TrimSpace(raw[len(prefix):]), true
	}
	return "", false
}
