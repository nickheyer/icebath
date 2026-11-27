package worktrees

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/worktrees/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/worktrees/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/worktrees/launch"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/worktrees/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/worktrees/tools"
)

func Register(registries *plugins.Registries) {
	trees := new(launch.Trees)
	server.Register(registries.Server, registries.Config, registries.Worktrees.Host(), registries.Sessions, registries.Terminal.Host())
	tools.Register(registries.Tools, registries.Worktrees.Host())
	cli.Register(registries.CLI)
	app.Register(registries.App, trees)
	launch.Register(registries.Launch, trees)
}
