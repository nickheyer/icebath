package cli

import (
	"github.com/nickheyer/icebath/internal/credentials"
	anthropiccredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/credentials"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
)

type signins struct{}

func Register(registry *providercli.Registry) {
	registry.Register(signins{})
}

// Sign-ins auth add runs for Anthropic: the subscription, claude setup-token, or Claude Code's own login
func (signins) SignIns() []credentials.SignIn {
	return []credentials.SignIn{anthropiccredentials.Subscription, anthropiccredentials.SetupToken, anthropiccredentials.ClaudeCode}
}
