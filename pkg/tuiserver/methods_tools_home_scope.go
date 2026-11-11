package tuiserver

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Scope binding the session profile home and logical cwd, a named profile scoping a session-less call
func sessionHomeScope(session *tuigateway.Session, cwd, profile string) func(context.Context) (context.Context, error) {
	return func(ctx context.Context) (context.Context, error) {
		scopeHome := ""
		if session != nil {
			scopeHome = strOr(session.Get("profile_home"))
		}
		if session == nil && profile != "" {
			resolved, err := profileHome(profile)
			if err != nil {
				return nil, err
			}
			scopeHome = resolved
		}
		if cwd == "" && session != nil {
			cwd = strOr(session.Get("cwd"))
		}
		if scopeHome != "" {
			ctx = home.WithOverride(ctx, scopeHome)
		}
		if cwd != "" {
			ctx = withLogicalSessionCwd(ctx, cwd)
		}
		return ctx, nil
	}
}

// Context carrying a copy of its tool env with the logical session cwd pinned, the original env untouched
func withLogicalSessionCwd(ctx context.Context, cwd string) context.Context {
	env := &tools.Env{}
	if current := tools.ToolEnvFrom(ctx); current != nil {
		*env = *current
	}
	values := &sync.Map{}
	if env.Values != nil {
		env.Values.Range(func(k, v any) bool {
			values.Store(k, v)
			return true
		})
	}
	env.Values = values
	if env.Home == "" {
		env.Home = home.Dir(ctx)
	}
	tools.SetLogicalSessionCwd(env, cwd)
	return tools.WithToolEnv(ctx, env)
}
