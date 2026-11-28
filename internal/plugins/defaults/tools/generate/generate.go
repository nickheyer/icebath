package generate

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/generate/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/generate/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/generate/tools"
)

func Register(registries *plugins.Registries) {
	services := registries.Generation.Host()
	tools.Register(registries.Tools, services, registries.Terminal.Host())
	server.Register(registries.Server, registries.Config, services, registries.Provider)
	app.Register(registries.App, registries.Views)
}
