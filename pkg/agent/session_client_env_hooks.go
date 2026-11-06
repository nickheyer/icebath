package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type boundClientEnvKey struct{}

func init() {
	tools.SessionToolchainEnviron = func(env *tools.Env, base map[string]string) map[string]string {
		return state.ToolchainEnviron(boundClientEnvContext(env), base)
	}
	tools.SessionToolEnv = func(env *tools.Env) map[string]string { return state.ToolEnv(boundClientEnvContext(env)) }
}

// Context carrying the client env the running turn of the tool env bound, unbound when none was captured
func boundClientEnvContext(env *tools.Env) context.Context {
	ctx := context.Background()
	if env == nil || env.Values == nil {
		return ctx
	}
	v, _ := env.Values.Load(boundClientEnvKey{})
	if b, ok := v.(*state.ClientEnvBinding); ok && b != nil {
		return state.ClientEnvScope(ctx, b.Env, b.Local, b.Rules)
	}
	return ctx
}

// Captures the session client env bound on the turn context onto the agent tool env its tool calls read
func bindTurnClientEnv(ctx context.Context, a *AIAgent) {
	if a.env == nil || a.env.Values == nil {
		return
	}
	a.env.Values.Store(boundClientEnvKey{}, state.BoundClientEnvBinding(ctx))
}
