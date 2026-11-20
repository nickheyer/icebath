package mcp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Connects one server within a connect timeout in seconds, lists its tools and disconnects
func ProbeSingleServerTimeout(ctx context.Context, env *tools.Env, name string, cfg map[string]any, connectTimeout float64) ([]ToolSummary, error) {
	return probeServer(ctx, env, name, cfg, &connectTimeout, nil)
}
