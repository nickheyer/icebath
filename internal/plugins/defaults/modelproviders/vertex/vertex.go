package vertex

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	app.Register(registries.Connectors)
	cli.Register(registries.SignIns)
}
