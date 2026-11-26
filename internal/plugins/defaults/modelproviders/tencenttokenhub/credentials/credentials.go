package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	tencenttokenhubconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: tencenttokenhubconfig.TokenHubName, Variables: []string{tencenttokenhubconfig.TokenHubAPIKeyEnv}})
	registry.Register(credentials.Keyed{Provider: tencenttokenhubconfig.TokenPlanName, Variables: []string{tencenttokenhubconfig.TokenPlanAPIKeyEnv}})
}
