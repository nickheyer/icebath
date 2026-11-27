package clarify

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/clarify/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/clarify/tools"
)

func Register(registries *plugins.Registries) {
	tools.Register(registries.Tools)
	app.Register(registries.Views)
}
