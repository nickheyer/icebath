package files

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	packages.Register(registries.Packages)
	tools.Register(registries.Tools, registries.Profile, registries.Terminal.Host(), registries.Web, registries.Skills, registries.Packages.Host())
	app.Register(registries.Views)
}
