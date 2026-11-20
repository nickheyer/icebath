package gateway

import (
	"context"
)

// Owner with no launch slots, the getattr default a missing authority reads as
type absentLaunchOwner struct{}

func (absentLaunchOwner) launchOwnerDBPath() string             { return "" }
func (absentLaunchOwner) launchOwnerProfileID() string          { return "" }
func (absentLaunchOwner) launchSecretSlots() *launchSecretSlots { return nil }

// Next turn's config and model of the policy as the given authority reads them, secrets recovered when asked
func (p *LocalSessionPolicy) EffectiveFor(ctx context.Context, authority *SessionAuthority, secrets bool) (map[string]any, *string, error) {
	if authority == nil {
		return p.effective(ctx, nil, secrets)
	}
	return p.effective(ctx, authority, secrets)
}

// Launch key a turn on the config sends under the authority, nil once the route left the bound endpoint
func RouteLaunchKey(ctx context.Context, authority *SessionAuthority, policy *LocalSessionPolicy, config map[string]any) (*string, error) {
	if authority == nil {
		return routeLaunchKey(ctx, absentLaunchOwner{}, policy, config)
	}
	return routeLaunchKey(ctx, authority, policy, config)
}
