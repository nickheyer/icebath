package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Liveness answer the cron profile gate reads, replaced by tests
var cronGateLiveness = gateway.ResolveGatewayLiveness

// Profile homes the in process cron ticker visits, the served set plus the launch profile
func cronTickProfileHomes(ctx context.Context, cfg *gateway.GatewayConfig) ([]profiles.ServedProfile, error) {
	homes, err := gateway.MultiplexProfileHomes(ctx, cfg)
	if err != nil {
		return nil, err
	}
	active := profiles.GetActiveProfileName(ctx)
	if active == "" {
		active = "default"
	}
	for _, p := range homes {
		if p.Name == active {
			return homes, nil
		}
	}
	dir, err := profiles.GetProfileDir(active)
	if err != nil {
		return homes, nil
	}
	return append(homes, profiles.ServedProfile{Name: active, Home: dir}), nil
}

// Routes each profile's log records to its own logs directory when multiplexing, false when inert
func enableMultiplexLogRouting(ctx context.Context, cfg *gateway.GatewayConfig) bool {
	if cfg == nil || cfg.MultiplexProfiles == nil || !*cfg.MultiplexProfiles {
		return false
	}
	homes, err := gateway.MultiplexProfileHomes(ctx, cfg)
	if err != nil {
		pkgLog().Debug("could not enable per-profile log routing", "error", err)
		return false
	}
	paths := make([]string, 0, len(homes))
	for _, p := range homes {
		paths = append(paths, p.Home)
	}
	enabled, err := logging.EnableProfileLogRouting(paths)
	if err != nil {
		pkgLog().Debug("could not enable per-profile log routing", "error", err)
		return false
	}
	return enabled
}
