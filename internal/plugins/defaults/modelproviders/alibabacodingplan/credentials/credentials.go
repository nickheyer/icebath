package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	alibabacodingplanconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibabacodingplan/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: alibabacodingplanconfig.IntlName, Variables: []string{alibabacodingplanconfig.CodingPlanAPIKeyEnv, alibabacodingplanconfig.DashScopeAPIKeyEnv}})
	registry.Register(credentials.Keyed{Provider: alibabacodingplanconfig.CNName, Variables: []string{alibabacodingplanconfig.CodingPlanCNAPIKeyEnv, alibabacodingplanconfig.CodingPlanAPIKeyEnv, alibabacodingplanconfig.DashScopeAPIKeyEnv}})
}
