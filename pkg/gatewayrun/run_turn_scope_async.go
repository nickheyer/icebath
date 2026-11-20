package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
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
