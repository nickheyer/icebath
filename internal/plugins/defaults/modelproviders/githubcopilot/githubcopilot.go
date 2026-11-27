package githubcopilot

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
	app.Register(registries.Connectors)
	cli.Register(registries.SignIns)
}
