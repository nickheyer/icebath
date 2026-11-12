package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Model and runtime the owner runs one turn on for a committed model override
func OneTurnRuntime(ctx context.Context, override *pyjson.Object, model string, runtimeKwargs map[string]any, cfg map[string]any) (string, map[string]any, error) {
	return oneTurnRuntime(ctx, override, model, runtimeKwargs, cfg)
}
