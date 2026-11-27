package compressor

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/contexts/compressor/auxiliary"
	"github.com/nickheyer/icebath/internal/plugins/defaults/contexts/compressor/engine"
)

func Register(registries *plugins.Registries) {
	engine.Register(registries.Contexts)
	auxiliary.Register(registries.Auxiliary)
}
