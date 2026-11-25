package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	alibabaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/alibaba/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: alibabaconfig.IntlName, Key: alibabaconfig.IntlAPIKey, Variables: []string{alibabaconfig.DashScopeAPIKeyEnv}})
	registry.Register(credentials.Keyed{Config: cfg, Provider: alibabaconfig.CNName, Key: alibabaconfig.CNAPIKey, Variables: []string{alibabaconfig.DashScopeAPIKeyEnv}})
	registry.Register(credentials.Keyed{Config: cfg, Provider: alibabaconfig.TokenPlanName, Key: alibabaconfig.TokenPlanAPIKey, Variables: []string{alibabaconfig.TokenPlanAPIKeyEnv}})
	registry.Register(credentials.Keyed{Config: cfg, Provider: alibabaconfig.TokenPlanCNName, Key: alibabaconfig.TokenPlanCNAPIKey, Variables: []string{alibabaconfig.TokenPlanCNAPIKeyEnv, alibabaconfig.TokenPlanAPIKeyEnv}})
}
