package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Runs body once per served profile inside its runtime scope, once under the launch scope when not multiplexing
func forEachServedProfile(ctx context.Context, r *GatewayRunner, body func(ctx context.Context, label string) error) error {
	if r == nil || !r.multiplexing() {
		return body(provider.WithLaunchProfileScopeIfMultiplexed(ctx), "default")
	}
	served, err := gateway.MultiplexProfileHomes(ctx, r.config)
	if err != nil {
		return err
	}
	for _, p := range served {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Housekeeping for profile %s skipped: %v", p.Name, rec))
				}
			}()
			// A profile unserved since the listing keeps its home untouched, and its release waits for this pass
			leave, held := gateway.ProcessOwnership.Hold(p.Home)
			if !held {
				return
			}
			defer leave()
			if err := body(gateway.ProfileRuntimeScope(ctx, p.Home, true), p.Name); err != nil {
				runLog().Debug(fmt.Sprintf("Housekeeping for profile %s skipped: %s", p.Name, pyrepr.ExceptionMessage(err)))
			}
		}()
	}
	return nil
}

// Zero argument chore run once per served profile under that profile's scope
func profileScopedChore(ctx context.Context, r *GatewayRunner, chore func(ctx context.Context) error) func() error {
	return func() error {
		return forEachServedProfile(ctx, r, func(ctx context.Context, _ string) error { return chore(ctx) })
	}
}
