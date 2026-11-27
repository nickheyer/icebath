package cli

import (
	"github.com/nickheyer/icebath/internal/credentials"
	minimaxcredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/credentials"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
)

type signins struct{}

func Register(registry *providercli.Registry) {
	registry.Register(signins{})
}

// The MiniMax sign-ins auth add runs, the China portal's for minimax-cn or --type oauth-cn
func (signins) SignIns() []credentials.SignIn {
	return []credentials.SignIn{minimaxcredentials.SignIn, minimaxcredentials.SignInCN}
}
