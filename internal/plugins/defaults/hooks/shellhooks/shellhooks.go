package shellhooks

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/hooks"
	"github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/launch"
	"github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/server"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	shell := hooks.Register(registries.Hooks, registries.Packages.Host(), registries.Terminal.Host())
	server.Register(registries.Server, registries.Config, shell, registries.Hooks.Host())
	cli.Register(registries.CLI)
	launch.Register(registries.Launch)
	app.Register(registries.App)
}
