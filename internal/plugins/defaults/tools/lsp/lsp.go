package lsp

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	manager := tools.Register(registries.Config, registries.Tools, registries.Profile, registries.Sessions, registries.Worktrees, registries.Terminal.Host(),
		registries.Packages.Host())
	server.Register(registries.Server, registries.Config, manager)
	cli.Register(registries.CLI)
	app.Register(registries.App)
}
