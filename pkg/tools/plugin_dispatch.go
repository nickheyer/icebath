package tools

import (
	"context"
	"encoding/json"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
)

type parentAgentKey struct{}

// Binds the parent agent a dispatched tool may hand its children, the parent_agent handler kwarg
func WithParentAgent(ctx context.Context, agent any) context.Context {
	return context.WithValue(ctx, parentAgentKey{}, agent)
}

// Parent agent bound to the context, nil when none was bound
func ParentAgentFrom(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	return ctx.Value(parentAgentKey{})
}

// Tool environment of a profile scope, the bound session env rebound to the scope home when it names another
func pluginScopeEnv(ctx context.Context, scope string) *Env {
	env := ToolEnvFrom(ctx)
	if env == nil {
		cfg, _ := config.LoadReadonly(ctx, scope, nil)
		cwd, _ := os.Getwd()
		return &Env{Home: scope, Config: cfg, Secrets: config.NewSecrets(scope), Cwd: cwd, Log: pkgLog()}
	}
	if scope == "" || ScopeKey(env.Home) == ScopeKey(scope) {
		return env
	}
	bound := *env
	bound.Home = scope
	bound.Secrets = config.NewSecrets(scope)
	return &bound
}

// Text of a dispatch kwarg, empty for None
func kwargText(kwargs map[string]any, key string) string {
	s, _ := kwargs[key].(string)
	return s
}

// Executes a tool handler by name in a profile scope for a plugin, the context kwargs riding the call
func DispatchForPlugin(ctx context.Context, scope, toolName string, args, kwargs map[string]any) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	if parent, ok := kwargs["parent_agent"]; ok && parent != nil {
		ctx = WithParentAgent(ctx, parent)
	}
	call := &Call{Name: toolName, Args: args, RawArgs: string(raw), Env: pluginScopeEnv(ctx, scope),
		TaskID: kwargText(kwargs, "task_id"), SessionID: kwargText(kwargs, "session_id"), UserTask: kwargText(kwargs, "user_task"),
		ToolCallID: kwargText(kwargs, "tool_call_id"), TurnID: kwargText(kwargs, "turn_id"), APIRequestID: kwargText(kwargs, "api_request_id")}
	return DispatcherOf(ctx).RegistryDispatch(ctx, call), nil
}
