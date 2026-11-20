package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Adapters of the launch profile, empty when none are bound
func (r *GatewayRunner) primaryAdapters() *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	return r.adapters
}

// Adapter maps of the served secondary profiles, empty when none are bound
func (r *GatewayRunner) profileAdaptersMap() *pydict.Dict[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]] {
	return r.profileAdapters
}

// Whether a live adapter is registered and the secondary profile owning it, empty for the primary
func (r *GatewayRunner) owningProfile(adapter gateway.PlatformAdapter, platform gateway.Platform) (bool, *string) {
	if primary, ok := r.primaryAdapters().Get(platform); ok && primary == adapter {
		return true, nil
	}
	for profile, adapters := range r.profileAdaptersMap().All() {
		if owned, ok := adapters.Get(platform); ok && owned == adapter {
			return true, &profile
		}
	}
	return false, nil
}

// Registered adapter that created source and its profile, false when it is gone or unregistered
func (r *GatewayRunner) transportOwner(source *gateway.SessionSource) (gateway.PlatformAdapter, *string, bool) {
	if source == nil {
		return nil, nil, false
	}
	adapter := source.TransportAdapterRef()
	if adapter == nil || source.Platform == "" {
		return nil, nil, false
	}
	registered, profile := r.owningProfile(adapter, source.Platform)
	if !registered {
		return nil, nil, false
	}
	return adapter, profile, true
}

// Adapter that created source and its profile name, empty for the primary profile
func (r *GatewayRunner) TransportOwner(source *gateway.SessionSource) (gateway.PlatformAdapter, string, bool) {
	adapter, profile, ok := r.transportOwner(source)
	if profile == nil {
		return adapter, "", ok
	}
	return adapter, *profile, ok
}

var _ gateway.TransportOwnerRunner = (*GatewayRunner)(nil)
