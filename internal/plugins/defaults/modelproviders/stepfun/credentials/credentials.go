package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	stepfunconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: stepfunconfig.Name, Variables: []string{stepfunconfig.APIKeyEnv}})
}
