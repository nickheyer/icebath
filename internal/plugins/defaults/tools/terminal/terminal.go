package terminal

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/prompt"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/slash"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/tools"
)

func Register(registries *plugins.Registries) {
	host := registries.Terminal.Host()
	config.Register(registries.Config)
	tools.Register(registries.Tools, registries.Config, host, registries.Approvals.Host(), registries.Skills, registries.Hooks.Host())
	prompt.Register(registries.Prompt, host, registries.Desktop.Host())
	slash.Register(registries.Slash, registries.Config, registries.Sessions, host, registries.Tools)
	app.Register(registries.App, registries.Views)
	server.Register(registries.Server, registries.Config, registries.Sessions, host)
}
