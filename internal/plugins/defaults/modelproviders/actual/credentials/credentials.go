package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	actualconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/config"
	actualprovider "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

const placeholder = "dummy-actual-local-api-key"

type source struct{}

func Register(registry *credentials.Registry) {
	registry.Register(source{})
}

func (source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != actualconfig.Name {
		return credentials.Credential{}, false
	}
	base := actualprovider.NormalizeBaseURL(account.Provider.Spec.BaseURL)
	credential, ok := account.Lookup(providerconfig.APIKeyField, actualconfig.APIKeyEnv)
	if !ok {
		if !actualprovider.Loopback(base) {
			return credentials.Credential{}, false
		}
		credential = credentials.Credential{APIKey: placeholder, Origin: credentials.Unneeded}
	}
	credential.BaseURL = base
	return credential, true
}
