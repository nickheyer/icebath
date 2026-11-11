package tools

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Whether a toolset's required credentials are configured for the home of the context
func ToolsetHasKeys(ctx context.Context, tsKey string, cfg config.Tree) (bool, error) {
	setup := &ToolsSetup{Ctx: ctx, Env: &Env{Home: home.Dir(ctx), Config: cfg}}
	return setup.toolsetHasKeys(tsKey, cfg, false, nil)
}
