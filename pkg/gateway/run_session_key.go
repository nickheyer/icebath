package gateway

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner that exposes the gateway config its fallback session keys honor
type gatewayConfigRunner interface {
	GatewayConfig() *GatewayConfig
}

// Current session key of a source, the store's when it yields one else the key its config builds
func runnerSessionKeyForSource(ctx context.Context, runner AuthorityRunner, source *SessionSource) (string, error) {
	if store := runner.SessionStore(); store != nil {
		if key, err := store.generateSessionKey(ctx, source, nil); err == nil && key != "" {
			return key, nil
		}
	}
	groupPerUser, threadPerUser := true, false
	if holder, ok := runner.(gatewayConfigRunner); ok {
		if config := holder.GatewayConfig(); config != nil {
			groupPerUser, threadPerUser = config.GroupSessionsPerUser, config.ThreadSessionsPerUser
		}
	}
	profile := ""
	if identity := IdentityOf(source); identity != nil {
		profile = identity.SessionKeyProfile()
	} else if runner.MultiplexProfiles() {
		if source.Profile != nil && *source.Profile != "" {
			profile = *source.Profile
		} else if profile = profiles.GetActiveProfileName(ctx); profile == "" {
			profile = "default"
		}
	}
	return BuildSessionKey(ctx, source, groupPerUser, threadPerUser, profile)
}

// Platform, chat type, chat id and any named profile and thread of a session key, nil for other text
func parseSessionKey(sessionKey string) map[string]any {
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 5 || parts[0] != "agent" || !(parts[1] == "main" || parts[1] == "main~" || home.ProfileIDRe.MatchString(parts[1])) {
		return nil
	}
	result := map[string]any{"platform": parts[2], "chat_type": parts[3], "chat_id": parts[4]}
	if parts[1] != "main" {
		result["profile"] = state.ProfileFromSessionKeyNamespace(parts[1])
	}
	if len(parts) > 5 && (parts[3] == "dm" || parts[3] == "thread") {
		result["thread_id"] = parts[5]
	}
	return result
}
