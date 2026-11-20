package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Insertion ordered adapter map of alternating platform and adapter arguments
func adapterDict(pairs ...any) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	adapters := pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	for i := 0; i+1 < len(pairs); i += 2 {
		adapter, _ := pairs[i+1].(gateway.PlatformAdapter)
		adapters.Set(pairs[i].(gateway.Platform), adapter)
	}
	return adapters
}

// Insertion ordered profile adapter maps of alternating profile name and adapter map arguments
func profileAdapterDict(pairs ...any) *pydict.Dict[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]] {
	profiles := pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	for i := 0; i+1 < len(pairs); i += 2 {
		profiles.Set(pairs[i].(string), pairs[i+1].(*pydict.Dict[gateway.Platform, gateway.PlatformAdapter]))
	}
	return profiles
}
