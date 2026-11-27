package modal

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/modal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/modal/terminal"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	terminal.Register(registries.Terminal, registries.Skills)
}
