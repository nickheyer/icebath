package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

// Adapter of a platform bound to this runner so it resolves inbound profile routes before connecting
func (r *GatewayRunner) createAdapter(ctx context.Context, platform gateway.Platform, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := r.instantiateAdapter(ctx, platform, config)
	if err != nil {
		return nil, err
	}
	if adapter != nil {
		adapter.Base().GatewayRunner = r
	}
	return adapter, nil
}

// Adapter of a platform from the plugin registry first and then the builtin table
func (r *GatewayRunner) instantiateAdapter(ctx context.Context, platform gateway.Platform, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	if config != nil {
		if config.Extra == nil {
			config.Extra = map[string]any{}
		}
		if _, ok := config.Extra["group_sessions_per_user"]; !ok {
			config.Extra["group_sessions_per_user"] = r.config.GroupSessionsPerUser
		}
		if _, ok := config.Extra["thread_sessions_per_user"]; !ok {
			config.Extra["thread_sessions_per_user"] = r.config.ThreadSessionsPerUser
		}
	}
	created, registered, err := registryAdapter(ctx, platform, config)
	if err != nil {
		logSuppressed(slog.LevelDebug, err, "Platform registry lookup for '%s' failed: %s", platform)
	} else if registered {
		if created == nil {
			pkgLog().Error(fmt.Sprintf("Platform '%s' is registered but adapter creation failed (check dependencies and config)", platform))
			return nil, nil
		}
		adapter, ok := created.(gateway.PlatformAdapter)
		if !ok {
			return nil, &errorutil.Error{Kind: "AttributeError", Msg: fmt.Sprintf("the %s plugin created %s where a platform adapter was expected", platform, errchain.Kind(created))}
		}
		return adapter, nil
	}
	return gateway.InstantiateBuiltinAdapter(ctx, platform, config)
}

// Object the platform registry creates and whether the platform is registered, the raised error of the lookup
func registryAdapter(ctx context.Context, platform gateway.Platform, config *gateway.PlatformConfig) (created any, registered bool, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			created, registered, err = nil, false, fmt.Errorf("%s", anyutil.String(rec))
		}
	}()
	if !gateway.Registry.IsRegistered(ctx, string(platform)) {
		return nil, false, nil
	}
	return gateway.Registry.CreateAdapter(ctx, string(platform), config), true, nil
}
