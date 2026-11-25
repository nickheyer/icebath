package terminal

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminal/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	tools.Register(registries.Tools)
}
