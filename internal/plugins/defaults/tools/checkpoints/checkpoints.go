package checkpoints

import (
	checkpointsconfig "github.com/nickheyer/icebath/internal/checkpoints/config"
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/launch"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/profile"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/sessions"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/slash"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints/tools"
)

func Register(registries *plugins.Registries) {
	host := registries.Terminal.Host()
	checkpointsconfig.Register(registries.Config)
	forget := tools.Register(registries.Tools, host)
	sessions.Register(registries.Sessions, registries.Config, forget)
	slash.Register(registries.Slash, registries.Config, registries.Sessions, host)
	server.Register(registries.Server, registries.Config, registries.Sessions, host)
	cli.Register(registries.CLI)
	app.Register(registries.App)
	launch.Register(registries.Launch)
	profile.Register(registries.Profile, registries.Config)
}
