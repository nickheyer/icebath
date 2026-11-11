package gatewayrun

import (
	"context"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Liveness answer the cron profile gate reads, replaced by tests the way hermes tests monkeypatch gateway.status
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

// Whether the ticker visits home this cycle, false only when another gateway process owns it
func cronProfileGate(name, profileHome string) (tick bool) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("Cron profile gate probe failed for %s (ticking it): %v", name, rec))
			tick = true
		}
	}()
	liveness := cronGateLiveness(gateway.LivenessOptions{
		ProfileDir: profileHome, NoCache: true,
		PidProbe: func(path string) (int, bool, error) { return gateway.GetRunningPid(path, false) },
	})
	return !(liveness.Running && liveness.PID != nil && !gateway.PyValuesEqual(liveness.PID, int64(os.Getpid())))
}

// Routes each profile's log records to its own logs directory when multiplexing, false when inert
func enableMultiplexLogRouting(ctx context.Context, cfg *gateway.GatewayConfig) bool {
	if cfg == nil || cfg.MultiplexProfiles == nil || !*cfg.MultiplexProfiles {
		return false
	}
	homes, err := gateway.MultiplexProfileHomes(ctx, cfg)
	if err != nil {
		runLog().Debug("could not enable per-profile log routing", "error", err)
		return false
	}
	paths := make([]string, 0, len(homes))
	for _, p := range homes {
		paths = append(paths, p.Home)
	}
	enabled, err := logging.EnableProfileLogRouting(paths)
	if err != nil {
		runLog().Debug("could not enable per-profile log routing", "error", err)
		return false
	}
	return enabled
}
