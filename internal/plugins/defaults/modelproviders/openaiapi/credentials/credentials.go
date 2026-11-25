package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	openaiapiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: openaiapiconfig.Name, Key: openaiapiconfig.APIKey, Variables: []string{openaiapiconfig.APIKeyEnv}})
}
