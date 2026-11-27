package singularity

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/singularity/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/terminals/singularity/terminal"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	terminal.Register(registries.Terminal, registries.Skills)
}
