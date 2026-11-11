package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Profile home the source's scope binds under multiplexing, false when the standalone launch scope applies
func (r *GatewayRunner) profileScopeKeyForSource(ctx context.Context, source *gateway.SessionSource) (string, bool, error) {
	if r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles {
		h, err := r.resolveProfileHomeForSource(ctx, source)
		return h, true, err
	}
	return "", false, nil
}

// Standalone gateway work scope, the launch profile's own runtime once the process hosts another profile
func standaloneLaunchScope(ctx context.Context) context.Context {
	return provider.WithLaunchProfileScopeIfMultiplexed(ctx)
}

// Turn scope of the source's profile, its credentials and home bound, marked local Desktop work for a declared local chat
func (r *GatewayRunner) profileScopeForSource(ctx context.Context, source *gateway.SessionSource) (context.Context, error) {
	h, keyed, err := r.profileScopeKeyForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	scope := standaloneLaunchScope(ctx)
	if keyed {
		scope = gateway.ProfileRuntimeScope(ctx, h, true)
	}
	return gateway.LocalDesktopTurn(ctx, r, source, scope)
}
