package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	bedrockconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

type source struct{}

func Register(registry *credentials.Registry) {
	registry.Register(source{})
}

func (source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != bedrockconfig.Name {
		return credentials.Credential{}, false
	}
	credential, ok := credentials.Lookup(account.Fields, providerconfig.APIKeyField, bedrockconfig.APIKeyEnv)
	credential.Auth = provider.AuthAPIKey
	return credential, ok
}
