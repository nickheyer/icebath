package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	deepseekconfig "github.com/nickheyer/icebath/internal/plugins/defaults/deepseek/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: deepseekconfig.Name, Key: deepseekconfig.APIKey, Variables: []string{deepseekconfig.APIKeyEnv}})
}
