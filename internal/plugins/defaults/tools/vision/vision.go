package vision

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/vision/auxiliary"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/vision/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/vision/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	auxiliary.Register(registries.Auxiliary)
	tools.Register(registries.Tools)
}
