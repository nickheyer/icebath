package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Installs the profile aware runner lookup and the default profile fallback the plugin platform actions use
func init() {
	plugins.RunnerAuthorizationAdapter = func(runner any) (func(context.Context, string, string) any, bool) {
		resolver, ok := runner.(AuthorizationAdapterRunner)
		if !ok {
			return nil, false
		}
		return func(ctx context.Context, value, profile string) any {
			if adapter := resolver.AuthorizationAdapter(ctx, Platform(value), &profile); adapter != nil {
				return adapter
			}
			return nil
		}, true
	}
	plugins.RunnerAdapterByValue = func(runner any, value string) (any, bool) {
		registry, ok := runner.(primaryAdaptersRunner)
		if !ok {
			return nil, false
		}
		adapters := registry.Adapters()
		if adapters == nil {
			return nil, true
		}
		adapter, found := adapters.Get(Platform(value))
		if !found || adapter == nil {
			return nil, true
		}
		return adapter, true
	}
}
