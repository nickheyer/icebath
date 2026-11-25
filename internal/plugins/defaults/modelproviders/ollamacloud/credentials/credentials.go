package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	ollamacloudconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/ollamacloud/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: ollamacloudconfig.Name, Key: ollamacloudconfig.APIKey, Variables: []string{ollamacloudconfig.APIKeyEnv}})
}
