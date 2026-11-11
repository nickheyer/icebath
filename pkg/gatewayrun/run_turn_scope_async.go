package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Routed runtime scope of the source for slash handlers, the launch profile scope when not multiplexed
func (r *GatewayRunner) asyncProfileScopeForSource(ctx context.Context, source *gateway.SessionSource) (context.Context, error) {
	h, keyed, err := r.profileScopeKeyForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	if keyed {
		return gateway.ProfileRuntimeScope(ctx, h, true), nil
	}
	return provider.WithLaunchProfileScopeIfMultiplexed(ctx), nil
}

// Home and terminal policy scope validating a turn's media paths on the delivery side, no secrets hydrated
func (r *GatewayRunner) mediaDeliveryScopeForSource(ctx context.Context, source *gateway.SessionSource) (context.Context, error) {
	if !r.multiplexing() {
		return ctx, nil
	}
	h, err := r.resolveProfileHomeForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	scoped := home.WithOverride(ctx, h)
	scoped = provider.WithSecretScope(scoped, map[string]string{}, h)
	return tools.WithProfileTerminalScope(scoped, h, nil), nil
}
