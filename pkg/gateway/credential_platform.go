package gateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// True when the profile has a bot of its own, a local only adapter map holds no platform credential
func HasCredentialPlatform(adapters any, owned any) bool {
	if pyrepr.Truthy(owned) {
		return true
	}
	if m, ok := adapters.(map[string]any); ok {
		for platform := range m {
			if platform != "local" {
				return true
			}
		}
	}
	if m, ok := adapters.(*pydict.Dict[Platform, PlatformAdapter]); ok {
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
