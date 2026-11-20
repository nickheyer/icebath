package gateway

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Context bound to the session's frozen terminal policy with its launch cwd as the session cwd
func sessionWorkspace(ctx context.Context, runner AuthorityRunner, source *SessionSource) (context.Context, error) {
	policy, err := policyForSource(ctx, runner, source)
	if err != nil || policy == nil {
		return ctx, err
	}
	fields, err := loadsPolicyMap(policy.TerminalJSON)
	if err != nil {
		return ctx, err
	}
	terminal := tools.TerminalPolicy{}
	for key, value := range fields {
		terminal[key] = pyrepr.Str(value)
	}
	env := &tools.Env{}
	if current := tools.ToolEnvFrom(ctx); current != nil {
		*env = *current
	}
	values := &sync.Map{}
	if env.Values != nil {
		env.Values.Range(func(key, value any) bool {
			values.Store(key, value)
			return true
		})
	}
	env.Values = values
	if env.Home == "" {
		env.Home = home.Dir(ctx)
	}
	scope := &tools.TerminalScope{Policy: terminal}
	env.TerminalScope = scope
	tools.SetLogicalSessionCwd(env, policy.Cwd)
	return tools.WithToolEnv(tools.SetTerminalScope(ctx, scope), env), nil
}
