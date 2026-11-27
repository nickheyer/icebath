package docker

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/docker/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/docker/terminal"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	terminal.Register(registries.Terminal, registries.Skills)
}
