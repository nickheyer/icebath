package cli

import (
	"github.com/nickheyer/icebath/internal/credentials"
	copilotcredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/credentials"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
)

type signins struct{}

func Register(registry *providercli.Registry) {
	registry.Register(signins{})
}

// The GitHub device code sign-in auth add runs for Copilot
func (signins) SignIns() []credentials.SignIn {
	return []credentials.SignIn{copilotcredentials.SignIn}
}
