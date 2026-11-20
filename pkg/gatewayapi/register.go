package gatewayapi

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Publishes the api_server platform module, its adapter class and requirements probe
func init() {
	gateway.RegisterBuiltinPlatformModule("api_server", map[string]any{
		"APIServerAdapter": &gateway.AdapterClass{Name: "APIServerAdapter", Traits: apiServerAdapterTraits(),
			New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
				adapter, err := NewAPIServerAdapter(ctx, config)
				if err != nil {
					return nil, err
				}
				return adapter, nil
			}},
		"check_api_server_requirements": CheckAPIServerRequirements,
	})
}
