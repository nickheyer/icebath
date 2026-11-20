package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs body with the session variables of sc bound on a fresh turn env, clearing them on every exit
func (r *GatewayRunner) sessionEnvScope(ctx context.Context, sc *gateway.SessionContext, body func(env *tools.Env) error) error {
	env, err := r.setSessionEnv(ctx, sc)
	if err != nil {
		return err
	}
	defer r.clearSessionEnv(env)
	return body(env)
}
