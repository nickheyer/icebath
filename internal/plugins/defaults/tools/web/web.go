package web

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/web/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/web/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/web/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/web/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	tools.Register(registries.Tools, registries.Web, registries.Terminal.Host())
	server.Register(registries.Server, registries.Config, registries.Web)
	app.Register(registries.App)
}
