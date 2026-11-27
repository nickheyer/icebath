package contexts

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/contexts/compressor"
)

func Register(registries *plugins.Registries) {
	compressor.Register(registries)
}
