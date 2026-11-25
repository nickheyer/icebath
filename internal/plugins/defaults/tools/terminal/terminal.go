package terminal

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	tools.Register(registries.Tools)
	app.Register(registries.App)
}
