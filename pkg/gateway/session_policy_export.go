package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
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

// LocalDesktopTurn is the scope a turn enters, also marked as local Desktop work for a Desktop chat its creator declared local
func LocalDesktopTurn(ctx context.Context, runner AuthorityRunner, source *SessionSource, scope context.Context) (context.Context, error) {
	var policy *LocalSessionPolicy
	if source != nil && source.Platform == PlatformLocal {
		adapter, err := runner.AdapterForSource(ctx, source)
		if err != nil {
			return nil, err
		}
		if local, ok := adapter.(*LocalSessionAdapter); ok && local != nil {
			policy = local.policy(source.ChatID)
		}
	}
	if policy == nil || policy.Source != "gui" {
		return scope, nil
	}
	marked := false
	for _, raw := range []string{policy.ConfigJSON, policy.RequestJSON} {
		parsed, err := pyjson.Loads(raw)
		if err != nil {
			return nil, err
		}
		mapping, ok := pyjson.Plain(parsed).(map[string]any)
		if !ok {
			return nil, &PyError{Kind: "AttributeError", Msg: "'" + pyrepr.TypeName(parsed) + "' object has no attribute 'get'"}
		}
		if flag, isBool := mapping[LocalDesktopChat].(bool); isBool && flag {
			marked = true
			break
		}
	}
	if !marked {
		return scope, nil
	}
	return tools.WithServingLocalDesktop(scope, true), nil
}
