package generators

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/deepinfra"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/fal"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/krea"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/metaai"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/openai"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/openrouter"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/xai"
)

// In the order the automatic choice takes them when several are set up, FAL first as alpha's own service
func Register(registries *plugins.Registries) {
	fal.Register(registries)
	openai.Register(registries)
	xai.Register(registries)
	openrouter.Register(registries)
	deepinfra.Register(registries)
	krea.Register(registries)
	metaai.Register(registries)
}
