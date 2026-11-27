package local

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/local/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/local/terminal"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	terminal.Register(registries.Terminal, registries.Skills, registries.Packages.Host())
}
