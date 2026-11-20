package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Completion line of one finished tool call on the agent display, rendered by the tools display layer
func getCuteToolMessage(ctx context.Context, env *tools.Env, name string, args map[string]any, duration float64, result any) string {
	return tools.GetCuteToolMessage(ctx, env, name, args, duration, result)
}
