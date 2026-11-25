package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	alibabacodingplanconfig "github.com/nickheyer/icebath/internal/plugins/defaults/alibabacodingplan/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: alibabacodingplanconfig.IntlName, Key: alibabacodingplanconfig.IntlAPIKey, Variables: []string{alibabacodingplanconfig.CodingPlanAPIKeyEnv, alibabacodingplanconfig.DashScopeAPIKeyEnv}})
	registry.Register(credentials.Keyed{Config: cfg, Provider: alibabacodingplanconfig.CNName, Key: alibabacodingplanconfig.CNAPIKey, Variables: []string{alibabacodingplanconfig.CodingPlanCNAPIKeyEnv, alibabacodingplanconfig.CodingPlanAPIKeyEnv, alibabacodingplanconfig.DashScopeAPIKeyEnv}})
}
