package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	alibabaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: alibabaconfig.IntlName, Variables: []string{alibabaconfig.DashScopeAPIKeyEnv}})
	registry.Register(credentials.Keyed{Provider: alibabaconfig.CNName, Variables: []string{alibabaconfig.DashScopeAPIKeyEnv}})
	registry.Register(credentials.Keyed{Provider: alibabaconfig.TokenPlanName, Variables: []string{alibabaconfig.TokenPlanAPIKeyEnv}})
	registry.Register(credentials.Keyed{Provider: alibabaconfig.TokenPlanCNName, Variables: []string{alibabaconfig.TokenPlanCNAPIKeyEnv, alibabaconfig.TokenPlanAPIKeyEnv}})
}
