package ssh

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/ssh/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/ssh/terminal"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	terminal.Register(registries.Terminal, registries.Skills)
}
