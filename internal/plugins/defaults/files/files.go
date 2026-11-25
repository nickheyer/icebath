package files

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/files/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/files/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	tools.Register(registries.Tools)
}
