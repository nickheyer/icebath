package mcp

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/auxiliary"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	catalog "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/mcp"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/slash"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	auxiliary.Register(registries.Auxiliary)
	catalog.Register(registries.MCP)
	helpers := tools.Helpers{Tasks: registries.Auxiliary, Providers: registries.Provider, Transports: registries.Transport, Credentials: registries.Credentials,
		Packages: registries.Packages.Host(), Hooks: registries.Hooks.Host()}
	manager := tools.Register(registries.Tools, registries.Profile, registries.Config, registries.MCP, helpers, registries.Terminal.Host().Supervisor)
	server.Register(registries.Server, registries.Config, manager, registries.Sessions, registries.Tools, registries.Approvals.Host(),
		registries.Hooks.Host())
	slash.Register(registries.Slash, registries.Config, manager, registries.Sessions)
	app.Register(registries.App, registries.Views)
	cli.Register(registries.CLI)
}
