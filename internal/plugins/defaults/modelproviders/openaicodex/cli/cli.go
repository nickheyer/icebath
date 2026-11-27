package cli

import (
	"github.com/nickheyer/icebath/internal/credentials"
	codexcredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/credentials"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
)

type signins struct{}

func Register(registry *providercli.Registry) {
	registry.Register(signins{})
}

// The ChatGPT sign-in auth add runs for Codex
func (signins) SignIns() []credentials.SignIn {
	return []credentials.SignIn{codexcredentials.SignIn}
}
