package gateway

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Resolved skin dict of the profile in scope, empty when the skin engine fails
func resolvedSkin(ctx context.Context) (skin map[string]any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Debug("runtime.describe: skin resolution failed", "error", recovered)
			skin = map[string]any{}
		}
	}()
	cfg, err := config.LoadUserEffective(home.ConfigPath(home.Dir(ctx)), config.EnvRefLookupContext(ctx))
	if err != nil {
		slog.Debug("runtime.describe: skin resolution failed", "error", err)
		return map[string]any{}
	}
	return tuigateway.SkinPayload(ctx, cfg)
}
