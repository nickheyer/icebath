package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Logger records carry the gateway.run name of the runner module
func runLog() *slog.Logger { return slog.Default().With("logger", "gateway.run") }

// Text of an optional string, empty for None
func optText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Pointer to a copy of s, nil for the empty string
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Whether the runner config multiplexes every served profile
func (r *GatewayRunner) multiplexing() bool {
	return r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles
}

// Current session key of a source, the store derivation first, else the configured fallback key
func (r *GatewayRunner) sessionKeyForSource(ctx context.Context, source *gateway.SessionSource) (string, error) {
	if r.sessionStore != nil {
		if key, err := r.sessionStore.GenerateSessionKey(ctx, source); err == nil && key != "" {
			return key, nil
		}
	}
	profile := ""
	if identity := gateway.IdentityOf(source); identity != nil {
		profile = identity.SessionKeyProfile()
	} else if r.multiplexing() {
		if source.Profile != nil && *source.Profile != "" {
			profile = *source.Profile
		} else if name := profiles.GetActiveProfileName(ctx); name != "" {
			profile = name
		} else {
			profile = "default"
		}
	}
	group, thread := true, false
	if r.config != nil {
		group, thread = r.config.GroupSessionsPerUser, r.config.ThreadSessionsPerUser
	}
	return gateway.BuildSessionKey(ctx, source, group, thread, profile)
}

// The given session key, else the key derived from source, nil when it cannot be derived
func (r *GatewayRunner) resolveSessionKeyOrNone(ctx context.Context, source *gateway.SessionSource, sessionKey string) string {
	if sessionKey != "" || source == nil {
		return sessionKey
	}
	key, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return ""
	}
	return key
}

// Count of sessions holding a running turn slot
func (r *GatewayRunner) runningAgentCount() int {
	return r.runningAgents().Len()
}

// Restart while a restart was requested, else shutdown
func (r *GatewayRunner) statusActionLabel() string {
	if r.restartRequested {
		return "restart"
	}
	return "shutdown"
}

// Localized restarting or shutting down for the busy and drain notices
func (r *GatewayRunner) statusActionGerund(ctx context.Context) string {
	if r.restartRequested {
		return i18n.T(ctx, "gateway.busy.action_restarting", "", nil)
	}
	return i18n.T(ctx, "gateway.busy.action_shutting_down", "", nil)
}

// Identity set of every agent mid turn, pending sentinels left out
func (r *GatewayRunner) runningAgentIDs() map[*agent.AIAgent]struct{} {
	ids := map[*agent.AIAgent]struct{}{}
	for _, item := range r.runningAgentItems() {
		if item.agent != nil && item.agent != agentPendingSentinel {
			ids[item.agent] = struct{}{}
		}
	}
	return ids
}

// Running agents by session key, pending sentinels left out
func (r *GatewayRunner) snapshotRunningAgents() map[string]*agent.AIAgent {
	snapshot := map[string]*agent.AIAgent{}
	for _, item := range r.runningAgentItems() {
		if item.agent != agentPendingSentinel {
			snapshot[item.sessionKey] = item.agent
		}
	}
	return snapshot
}

// Profile name this gateway represents, default when none is active
func (r *GatewayRunner) activeProfileName(ctx context.Context) string {
	if name := profiles.GetActiveProfileName(ctx); name != "" {
		return name
	}
	return "default"
}

// Profile a configured route sends source to, nil for the default or the adapter's own profile
func (r *GatewayRunner) profileNameForSource(ctx context.Context, source *gateway.SessionSource, adapterProfile *string) (*string, error) {
	if !r.multiplexing() || len(r.config.ProfileRoutes) == 0 {
		return nil, nil
	}
	if adapterProfile == nil && source.TransportAdapterRef() != nil {
		if _, profile, owned := r.transportOwner(source); owned {
			adapterProfile = profile
		}
	}
	matched, err := gateway.MatchProfileRoute(ctx, r.config.ProfileRoutes, gateway.RouteQuery{
		Platform: string(source.Platform), GuildID: source.GuildID, ChatID: &source.ChatID,
		ThreadID: source.ThreadID, ParentChatID: source.ParentChatID, AdapterProfile: adapterProfile,
		UserID: source.UserID})
	if err != nil {
		runLog().Warn(fmt.Sprintf("Rejecting %s/%s: profile route matching failed: %v", source.Platform, source.ChatID, err))
		return nil, &gateway.ProfileRouteRejected{Args: []any{"matcher"}}
	}
	if matched != nil {
		served, err := gateway.MultiplexProfileHomes(ctx, r.config)
		if err != nil {
			runLog().Warn(fmt.Sprintf("Rejecting profile route %s because the served-profile set could not be resolved: %v",
				pyrepr.Repr(matched.Name), err))
			return nil, &gateway.ProfileRouteRejected{Args: []any{matched.Name}}
		}
		for _, profile := range served {
			if profile.Name == matched.Profile {
				return &matched.Profile, nil
			}
		}
		runLog().Warn(fmt.Sprintf("Rejecting profile route %s: target profile %s is not served",
			pyrepr.Repr(matched.Name), pyrepr.Repr(matched.Profile)))
		return nil, &gateway.ProfileRouteRejected{Args: []any{matched.Name, matched.Profile}}
	}
	runLog().Debug(fmt.Sprintf("No profile route matched: platform=%s chat_id=%s thread_id=%s parent_chat_id=%s",
		source.Platform, source.ChatID, pyrepr.Str(optAny(source.ThreadID)), pyrepr.Str(optAny(source.ParentChatID))))
	return nil, nil
}

// Optional string as a Python value, None for nil
func optAny(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Home of the profile serving source, the pinned identity home, then the source or routed profile, else the active one
func (r *GatewayRunner) resolveProfileHomeForSource(ctx context.Context, source *gateway.SessionSource) (string, error) {
	if identity := gateway.IdentityOf(source); identity != nil {
		return identity.RuntimeHome, nil
	}
	var explicit *string
	guild := pyrepr.Str(optAny(source.GuildID))
	name := strings.TrimSpace(optText(source.Profile))
	if name == "" {
		routed, err := r.profileNameForSource(ctx, source, nil)
		var rejected *gateway.ProfileRouteRejected
		if errors.As(err, &rejected) {
			return "", err
		}
		if err != nil {
			runLog().Warn(fmt.Sprintf("Failed to resolve profile directory for source %s/%s (guild_id=%s), "+
				"falling back to global ICEBATH_HOME: %s", source.Platform, source.ChatID, guild, "(no profile)"))
			return home.Dir(ctx), nil
		}
		name = optText(routed)
	}
	if name != "" {
		explicit = &name
	} else if name = profiles.GetActiveProfileName(ctx); name == "" {
		name = "default"
	}
	profileDir, err := profiles.GetProfileDir(name)
	if err != nil {
		label := "(no profile)"
		if explicit != nil {
			label = *explicit
		}
		runLog().Warn(fmt.Sprintf("Failed to resolve profile directory for source %s/%s (guild_id=%s), "+
			"falling back to global ICEBATH_HOME: %s", source.Platform, source.ChatID, guild, label))
		return home.Dir(ctx), nil
	}
	if explicit != nil && !profiles.ProfileExists(name) {
		runLog().Warn(fmt.Sprintf("Profile %s does not exist for source %s/%s (guild_id=%s), "+
			"falling back to global ICEBATH_HOME", pyrepr.Repr(*explicit), source.Platform, source.ChatID, guild))
		return home.Dir(ctx), nil
	}
	return profileDir, nil
}

// Ordered distinct channel override keys, the exact id first, then the thread and the parent
func channelOverrideLookupKeys(chatID string, threadID, parentID *string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, key := range []string{chatID, optText(threadID), optText(parentID)} {
		if key != "" && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

// Channel override of a chat, then its thread, then its parent, nil when absent
func getChannelOverride(config *gateway.GatewayConfig, platform gateway.Platform, chatID string, threadID, parentID *string) *gateway.ChannelOverride {
	if config == nil || len(config.Platforms) == 0 {
		return nil
	}
	platformConfig := config.Platforms[platform]
	if platformConfig == nil || len(platformConfig.ChannelOverrides) == 0 {
		return nil
	}
	for _, key := range channelOverrideLookupKeys(chatID, threadID, parentID) {
		if override := platformConfig.ChannelOverrides[key]; override != nil {
			return override
		}
	}
	return nil
}

// Reasoning override of the state as session info reads it
func (s *SessionState) ReasoningOverride() map[string]any {
	return s.conversation.reasoningOverride
}

// The /fast override of the state and whether one is set
func (s *SessionState) ServiceTierOverride() (*string, bool) {
	return s.conversation.serviceTierOverride.tier, s.conversation.serviceTierOverride.set
}

// State of a route without creating one, nil when absent
func (r *GatewayRunner) PeekSessionState(route string) gateway.PeekedSessionState {
	if state := r.peekSessionState(route); state != nil {
		return state
	}
	return nil
}

// Store routing sessions and their transcripts
func (r *GatewayRunner) SessionStore() *gateway.SessionStore { return r.sessionStore }

// Whether the runner is draining
func (r *GatewayRunner) Draining() bool { return r.draining.Load() }

// Whether the runner is running
func (r *GatewayRunner) Running() bool { return r.running.Load() }

// Whether the runner multiplexes every served profile
func (r *GatewayRunner) MultiplexProfiles() bool { return r.multiplexing() }

// Copy of a reasoning override for storage
func cloneReasoning(cfg map[string]any) map[string]any {
	if cfg == nil {
		return nil
	}
	return maps.Clone(cfg)
}

var (
	_ gateway.AuthorityRunner = (*GatewayRunner)(nil)
	_ gateway.DrainRunner     = (*GatewayRunner)(nil)
)
