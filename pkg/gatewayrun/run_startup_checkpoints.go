package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Replays every served secondary profile's process checkpoint under its own scope, returning how many were re adopted
func (r *GatewayRunner) recoverSecondaryProcessCheckpoints(ctx context.Context, registry *tools.ProcessRegistry) (int, error) {
	if !r.multiplexing() {
		return 0, nil
	}
	launchHome, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return 0, err
	}
	served, err := gateway.MultiplexProfileHomes(ctx, r.config)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, profile := range served {
		resolved, err := pypath.Resolve(profile.Home)
		if err == nil && resolved == launchHome {
			continue
		}
		scoped := home.WithOverride(ctx, profile.Home)
		scoped = provider.WithSecretScope(scoped, map[string]string{}, profile.Home)
		scoped = tools.WithProfileTerminalScope(scoped, profile.Home, nil)
		count, err := registry.RecoverFromCheckpoint(scoped, &tools.Env{Home: home.Dir(scoped), TerminalScope: tools.GetTerminalScope(scoped)})
		if err != nil {
			runLog().Warn(fmt.Sprintf("Process checkpoint recovery for profile '%s' failed", profile.Name), "exc_info", err)
			continue
		}
		recovered += count
	}
	return recovered, nil
}
