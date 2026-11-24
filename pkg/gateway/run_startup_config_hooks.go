package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Registers the shell hooks and outbound webhooks of the current scope config, a failure logged at level
func RegisterWorkerConfigHooks(ctx context.Context, failureMessage string, level slog.Level) {
	if err := registerConfigHooks(ctx); err != nil {
		pkgLog().Log(ctx, level, failureMessage, "exc_info", err)
	}
}

// Wires the consented shell hooks then the outbound webhook targets of the scope's profile
func registerConfigHooks(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	if _, err := plugins.RegisterShellHooksFromConfig(ctx, false, false); err != nil {
		return err
	}
	_, err = plugins.RegisterOutboundWebhooksFromConfig(ctx)
	return err
}
