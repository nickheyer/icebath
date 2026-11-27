package cli

import (
	"github.com/nickheyer/icebath/internal/credentials"
	azurefoundrycredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/credentials"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
)

type signins struct{}

func Register(registry *providercli.Registry) {
	registry.Register(signins{})
}

// The Microsoft Entra ID sign-in auth add azure-foundry --type oauth takes
func (signins) SignIns() []credentials.SignIn {
	return []credentials.SignIn{azurefoundrycredentials.Entra}
}
