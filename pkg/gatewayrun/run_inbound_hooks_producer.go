package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Producer path pre gateway dispatch hook, nil to drop the delivery, else the event or its rewrite
func (r *GatewayRunner) HMPreGatewayDispatchHook(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource) *gateway.MessageEvent {
	return r.hmPreGatewayDispatchHook(ctx, event, source)
}

// Post admission plugin hook of a producer delivery, whether a plugin consumed it and the reply it gave
func (r *GatewayRunner) RunPostAdmissionHook(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string) (bool, *string, error) {
	return runPostAdmissionHook(ctx, r, event, source, sessionKey)
}

var _ gateway.ProducerHooksRunner = (*GatewayRunner)(nil)
