package apluginexample

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/examples/apluginexample/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/examples/apluginexample/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/examples/apluginexample/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/examples/apluginexample/server"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	server.Register(registries.Server, registries.Config)
	app.Register(registries.App)
	cli.Register(registries.CLI)
}
