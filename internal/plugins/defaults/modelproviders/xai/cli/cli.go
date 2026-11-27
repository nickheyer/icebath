package cli

import (
	"github.com/nickheyer/icebath/internal/credentials"
	xaicredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/credentials"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
)

type signins struct{}

func Register(registry *providercli.Registry) {
	registry.Register(signins{})
}

// The Grok sign-in auth add runs for xAI OAuth
func (signins) SignIns() []credentials.SignIn {
	return []credentials.SignIn{xaicredentials.SignIn}
}
