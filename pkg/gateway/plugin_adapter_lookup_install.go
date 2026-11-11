package gateway

import "github.com/nickheyer/icebath/pkg/plugins"

// Installs the default profile adapter lookup the plugin platform actions fall back to
func init() {
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
