package cli

import (
	"github.com/nickheyer/icebath/internal/credentials"
	openroutercredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/credentials"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
)

type signins struct{}

func Register(registry *providercli.Registry) {
	registry.Register(signins{})
}

// The OpenRouter sign-in auth add --type oauth runs, which gives the account an API key
func (signins) SignIns() []credentials.SignIn {
	return []credentials.SignIn{openroutercredentials.SignIn}
}
