package gateway

import (
	"context"
	"fmt"
	"maps"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Launch secret owner of the active authority, a nil owner when the runner has none
func activeOwner(ctx context.Context, runner any) (launchSecretOwner, error) {
	authority, err := activeAuthority(ctx, runner)
	if err != nil || authority == nil {
		return nil, err
	}
	return authority, nil
}

// Peeked state of a route, nil when the runner holds none or has no peek as the getattr read allows
func peekRouteState(runner any, sessionKey string) peekedSessionState {
	peeker, ok := runner.(sessionStatePeekRunner)
	if !ok || sessionKey == "" {
		return nil
	}
	return peeker.PeekSessionState(sessionKey)
}

// Next turn reasoning, a typed session override, the profile current effort when following, else the frozen one
func (p *LocalSessionPolicy) CurrentReasoningConfig(ctx context.Context, runner any, sessionKey string) (map[string]any, error) {
	if state := peekRouteState(runner, sessionKey); state != nil {
		if override := state.ReasoningOverride(); override != nil {
			return maps.Clone(override), nil
		}
	}
	if !p.FollowsProfile() {
		return p.ReasoningConfig(), nil
	}
	owner, err := activeOwner(ctx, runner)
	if err != nil {
		return nil, err
	}
	cfg, model, err := p.effective(ctx, owner, false)
	if err != nil {
		return nil, err
	}
	name := ""
	if model != nil {
		name = *model
	}
	return config.ResolveReasoningConfig(cfg, name), nil
}

// Next turn tier, a typed session /fast, else the policy tier frozen or followed from the profile
func (p *LocalSessionPolicy) CurrentServiceTier(ctx context.Context, runner any, sessionKey string) (*string, error) {
	if sessionKey != "" {
		peeker, ok := runner.(sessionStatePeekRunner)
		if !ok {
			return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_peek_session_state'", runner)}
		}
		if state := peeker.PeekSessionState(sessionKey); state != nil {
			if tier, set := state.ServiceTierOverride(); set {
				return tier, nil
			}
		}
	}
	owner, err := activeOwner(ctx, runner)
	if err != nil {
		return nil, err
	}
	cfg, err := p.config(ctx, owner, false)
	if err != nil {
		return nil, err
	}
	agentSection, _ := cfg["agent"].(map[string]any)
	raw := ""
	if value := agentSection["service_tier"]; value != nil {
		raw = pyrepr.Str(value)
	}
	tier := provider.ParseServiceTier(raw)
	if tier == "" {
		return nil, nil
	}
	return &tier, nil
}
