package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	customconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

type source struct{}

func Register(registry *credentials.Registry) {
	registry.Register(source{})
}

// API key of the account, else of the variable the endpoint names
func (source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != customconfig.Name {
		return credentials.Credential{}, false
	}
	return credentials.Lookup(account.Fields, providerconfig.APIKeyField, account.Provider.Spec.KeyEnv...)
}
