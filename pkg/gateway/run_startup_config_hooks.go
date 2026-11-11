package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Registers the shell hooks and outbound webhooks of the current scope config, a failure logged at level
func RegisterWorkerConfigHooks(ctx context.Context, failureMessage string, level slog.Level) {
	if err := registerConfigHooks(ctx); err != nil {
		slog.Default().With("logger", "gateway.run").Log(ctx, level, failureMessage, "exc_info", err)
	}
}

// Loads the scope config and wires its consented shell hooks then its outbound webhook targets
func registerConfigHooks(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	cfg, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		return err
	}
	if _, err := plugins.RegisterShellHooksFromConfig(ctx, map[string]any(cfg), false, false); err != nil {
		return err
	}
	_, err = plugins.RegisterOutboundWebhooksFromConfig(ctx, map[string]any(cfg))
	return err
}
