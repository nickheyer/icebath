package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	stepfunconfig "github.com/nickheyer/icebath/internal/plugins/defaults/stepfun/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: stepfunconfig.Name, Key: stepfunconfig.APIKey, Variables: []string{stepfunconfig.APIKeyEnv}})
}
