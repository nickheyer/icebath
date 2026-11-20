package profiles

import "context"

// Gateway platform facts pkg/gateway installs in init, entries and steps as attribute name mappings
var GatewayChannelHooks struct {
	PlatformMembers      func() []string
	PlatformEntries      func(ctx context.Context) ([]map[string]any, error)
	EnvEnableCredentials func() [][2]any
}
