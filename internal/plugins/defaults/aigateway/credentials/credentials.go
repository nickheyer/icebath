package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	aigatewayconfig "github.com/nickheyer/icebath/internal/plugins/defaults/aigateway/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: aigatewayconfig.Name, Key: aigatewayconfig.APIKey, Variables: []string{aigatewayconfig.APIKeyEnv}})
}
