package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Runner config with an unset multiplex flag settled first, reloaded under the default root scope when multiplexing
func loadGatewayConfigForRunner(ctx context.Context) (*gateway.GatewayConfig, error) {
	cfg, err := gateway.LoadGatewayConfig(ctx)
	if err != nil {
		return nil, err
	}
	decision, err := ResolveMultiplexMode(ctx, cfg)
	if err != nil {
		return nil, err
	}
	gateway.LogMultiplexDecision(ctx, decision)
	if cfg.MultiplexProfiles == nil || !*cfg.MultiplexProfiles {
		return cfg, nil
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("could not set multiplex-active before primary config load", "exc_info", rec)
			}
		}()
		provider.SetMultiplexActive(true)
	}()
	root, ok := func() (root string, ok bool) {
		defer func() {
			if rec := recover(); rec != nil {
				ok = false
			}
		}()
		return home.Root(), true
	}()
	if !ok {
		return cfg, nil
	}
	scoped, err := func() (scoped *gateway.GatewayConfig, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return gateway.LoadGatewayConfig(gateway.ProfileRuntimeScope(ctx, root, true))
	}()
	if err != nil {
		runLog().Debug("multiplex default-scope config reload failed; using unscoped load", "exc_info", err)
		return cfg, nil
	}
	scoped.MultiplexProfiles = cfg.MultiplexProfiles
	return scoped, nil
}
