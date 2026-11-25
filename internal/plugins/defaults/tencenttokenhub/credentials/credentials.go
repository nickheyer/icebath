package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	tencenttokenhubconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tencenttokenhub/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: tencenttokenhubconfig.TokenHubName, Key: tencenttokenhubconfig.TokenHubAPIKey, Variables: []string{tencenttokenhubconfig.TokenHubAPIKeyEnv}})
	registry.Register(credentials.Keyed{Config: cfg, Provider: tencenttokenhubconfig.TokenPlanName, Key: tencenttokenhubconfig.TokenPlanAPIKey, Variables: []string{tencenttokenhubconfig.TokenPlanAPIKeyEnv}})
}
