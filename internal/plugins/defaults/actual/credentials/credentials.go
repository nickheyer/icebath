package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	actualconfig "github.com/nickheyer/icebath/internal/plugins/defaults/actual/config"
)

const placeholder = "dummy-actual-local-api-key"

type source struct {
	config *config.Config
}

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(source{config: cfg})
}

func (s source) Credential(name string) (credentials.Credential, bool) {
	if name != actualconfig.Name {
		return credentials.Credential{}, false
	}
	base := actualconfig.NormalizeBaseURL(s.config.GetString(actualconfig.BaseURL))
	credential, ok := credentials.Lookup(s.config, actualconfig.APIKey, actualconfig.APIKeyEnv)
	if !ok {
		if !actualconfig.Loopback(base) {
			return credentials.Credential{}, false
		}
		credential = credentials.Credential{APIKey: placeholder, Source: "local-offline"}
	}
	credential.BaseURL = base
	return credential, true
}
