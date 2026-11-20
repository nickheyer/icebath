package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Model and agent kwargs of a policy's route, its fallback chain walked only when asked
func (r *GatewayRunner) ResolvePolicyAgentRuntime(ctx context.Context, policy *gateway.LocalSessionPolicy, fallback bool) (string, map[string]any, error) {
	return resolvePolicyAgentRuntime(ctx, r, policy, fallback)
}
