package gateway

import (
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
)

// True when the profile has a bot of its own, a local only adapter map holds no platform credential
func HasCredentialPlatform(adapters any, owned any) bool {
	if anyutil.IsTrue(owned) {
		return true
	}
	if m, ok := adapters.(map[string]any); ok {
		for platform := range m {
			if platform != "local" {
				return true
			}
		}
	}
	if m, ok := adapters.(*orderedutil.Map[Platform, PlatformAdapter]); ok {
		for _, platform := range m.Keys() {
			if platform != PlatformLocal {
				return true
			}
		}
	}
	if m, ok := adapters.(map[Platform]PlatformAdapter); ok {
		for platform := range m {
			if platform != PlatformLocal {
				return true
			}
		}
	}
	return false
}
