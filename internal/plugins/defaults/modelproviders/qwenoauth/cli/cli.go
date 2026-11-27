package cli

import (
	"github.com/nickheyer/icebath/internal/credentials"
	qwencredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/credentials"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
)

type signins struct{}

func Register(registry *providercli.Registry) {
	registry.Register(signins{})
}

// The Qwen sign-ins auth add runs: the device code, or the Qwen CLI's own login
func (signins) SignIns() []credentials.SignIn {
	return []credentials.SignIn{qwencredentials.SignIn, qwencredentials.QwenCLI}
}
