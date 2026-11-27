package todo

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/todo/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/todo/tools"
)

func Register(registries *plugins.Registries) {
	tools.Register(registries.Tools, registries.Config, registries.Sessions)
	app.Register(registries.Views)
}
