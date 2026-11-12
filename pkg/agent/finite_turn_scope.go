package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Puts the admission finite and unattended flags on the env the tools read this turn, no op outside one
func bindAdmissionFiniteScope(ctx context.Context, env *tools.Env) (restore func()) {
	finite := gateway.FiniteTurnRequired(ctx)
	if finite == nil {
		return func() {}
	}
	return tools.BindFiniteTurn(env, finite, gateway.UnattendedTurn(ctx))
}
