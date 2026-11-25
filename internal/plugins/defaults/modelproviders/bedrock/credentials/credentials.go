package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	bedrockconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type source struct {
	config *config.Config
}

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(source{config: cfg})
}

func (s source) Credential(name string) (credentials.Credential, bool) {
	if name != bedrockconfig.Name {
		return credentials.Credential{}, false
	}
	credential, ok := credentials.Lookup(s.config, bedrockconfig.APIKey, bedrockconfig.APIKeyEnv)
	credential.Auth = provider.AuthAPIKey
	return credential, ok
}
