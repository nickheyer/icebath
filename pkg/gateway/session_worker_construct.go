package gateway

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Runner rereading the profile fallback chain for the next agent, GatewayRunner._refresh_fallback_model
type fallbackRefreshRunner interface {
	RefreshFallbackModel(ctx context.Context) []map[string]any
}

// Runner resolving a channel's ephemeral gateway prompt, GatewayRunner._get_system_prompt_for_channel
type channelPromptRunner interface {
	SystemPromptForChannel(ctx context.Context, platform Platform, chatID string, threadID, parentID *string) string
}

// Agent construction inputs the owner resolves per turn for a managed worker in the owning profile's scope
func constructInputs(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, chatID string) (*pyjson.Object, error) {
	scoped := ownerScope(ctx, a, false)
	chain, err := fallbackChain(scoped, a, policy)
	if err != nil {
		return nil, err
	}
	prompt, err := ephemeralPrompt(scoped, a, policy, chatID)
	if err != nil {
		return nil, err
	}
	prefill, err := sessionPrefill(scoped, a, policy, chatID)
	if err != nil {
		return nil, err
	}
	var prefillValue any
	if prefill != nil {
		prefillValue = prefill
	}
	return pyjson.ObjectOf("fallback_model", chain, "ephemeral_system_prompt", prompt, "prefill_messages", prefillValue), nil
}

// Fallback chain an in process turn gets, the frozen snapshot for a config only session and none in safe mode
func fallbackChain(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy) (any, error) {
	if policy.SafeMode {
		return nil, nil
	}
	if policy.IgnoreUserConfig {
		cfg, err := policy.config(ctx, a, true)
		if err != nil {
			return nil, err
		}
		if chain := provider.GetFallbackChain(cfg); len(chain) > 0 {
			return chain, nil
		}
		return nil, nil
	}
	refresher, ok := a.Runner.(fallbackRefreshRunner)
	if !ok {
		return nil, nil
	}
	if chain := refresher.RefreshFallbackModel(ctx); chain != nil {
		return chain, nil
	}
	return nil, nil
}

// Combined ephemeral prompt of a local turn, the gateway prompt then the frozen skills blocks, nil when both are empty
func ephemeralPrompt(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, chatID string) (any, error) {
	configured := ""
	if resolver, ok := a.Runner.(channelPromptRunner); ok && !policy.IgnoreUserConfig {
		scoped, err := policy.ClientEnvScope(ctx)
		if err != nil {
			return nil, err
		}
		configured = resolver.SystemPromptForChannel(scoped, PlatformLocal, chatID, nil, nil)
	}
	var parts []string
	if configured != "" {
		parts = append(parts, configured)
	}
	if policy.SkillsPrompt != nil && *policy.SkillsPrompt != "" {
		parts = append(parts, *policy.SkillsPrompt)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return strings.Join(parts, "\n\n"), nil
}

// Session's provider routing as of its creation from its own profile, never the runner's daemon start copy
func sessionRouting(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy) (map[string]any, error) {
	cfg, err := policy.config(ctx, a, false)
	if err != nil {
		return nil, err
	}
	if routing, ok := cfg["provider_routing"].(map[string]any); ok {
		return routing, nil
	}
	return map[string]any{}, nil
}
