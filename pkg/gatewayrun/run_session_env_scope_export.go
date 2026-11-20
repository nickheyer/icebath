package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs body with a session's turn variables bound, for owner verbs that act as that session's caller
func (r *GatewayRunner) SessionEnvScope(ctx context.Context, sc *gateway.SessionContext, body func(env *tools.Env) error) error {
	return r.sessionEnvScope(ctx, sc, body)
}
