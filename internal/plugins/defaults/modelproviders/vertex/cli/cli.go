package cli

import (
	"github.com/nickheyer/icebath/internal/credentials"
	vertexcredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/credentials"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
)

type signins struct{}

func Register(registry *providercli.Registry) {
	registry.Register(signins{})
}

// The service account key auth add vertex takes
func (signins) SignIns() []credentials.SignIn {
	return []credentials.SignIn{vertexcredentials.ServiceAccount}
}
