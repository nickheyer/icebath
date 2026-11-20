package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Home and terminal policy scope the adapter validates a turn's media paths in, unscoped without multiplexing
func (r *GatewayRunner) MediaDeliveryScopeForSource(ctx context.Context, source *gateway.SessionSource) (context.Context, func(), error) {
	if !r.MultiplexProfiles() {
		return ctx, func() {}, nil
	}
	profileHome, err := r.ResolveProfileHomeForSource(ctx, source)
	if err != nil {
		return ctx, nil, err
	}
	scoped := home.WithOverride(ctx, profileHome)
	scoped = provider.WithSecretScope(scoped, map[string]string{}, profileHome)
	return tools.WithProfileTerminalScope(scoped, profileHome, nil), func() {}, nil
}

var _ gateway.MediaDeliveryScopeRunner = (*GatewayRunner)(nil)
