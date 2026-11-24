package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
)

// Insertion ordered adapter map of alternating platform and adapter arguments
func adapterDict(pairs ...any) *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter] {
	adapters := orderedutil.NewMap[gateway.Platform, gateway.PlatformAdapter]()
	for i := 0; i+1 < len(pairs); i += 2 {
		adapter, _ := pairs[i+1].(gateway.PlatformAdapter)
		adapters.Set(pairs[i].(gateway.Platform), adapter)
	}
	return adapters
}

// Insertion ordered profile adapter maps of alternating profile name and adapter map arguments
func profileAdapterDict(pairs ...any) *orderedutil.Map[string, *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter]] {
	profiles := orderedutil.NewMap[string, *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter]]()
	for i := 0; i+1 < len(pairs); i += 2 {
		profiles.Set(pairs[i].(string), pairs[i+1].(*orderedutil.Map[gateway.Platform, gateway.PlatformAdapter]))
	}
	return profiles
}
