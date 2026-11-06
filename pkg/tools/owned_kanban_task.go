package tools

import (
	"context"
	"os"
	"strings"
)

type toolEnvKey struct{}

// Binds the session tool environment so context only callers read its session flags
func WithToolEnv(ctx context.Context, env *Env) context.Context {
	return context.WithValue(ctx, toolEnvKey{}, env)
}

// Session tool environment bound to the context, nil when none was bound
func ToolEnvFrom(ctx context.Context) *Env {
	if ctx == nil {
		return nil
	}
	env, _ := ctx.Value(toolEnvKey{}).(*Env)
	return env
}

// Board task this execution owns, ICEBATH_KANBAN_TASK for the dispatcher owned worker and empty otherwise
func OwnedKanbanTask(ctx context.Context) string {
	if IsDelegatedChildContext(ctx) || !isDispatcherOwnedWorkerContext(ToolEnvFrom(ctx)) {
		return ""
	}
	return strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_TASK"))
}
