package generate

import (
	"context"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	falconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/fal/config"
)

type services struct{}

func Register(registry *generation.Registry) {
	registry.Register(services{})
}

func (services) Services() []generation.Service {
	return []generation.Service{{
		Name:  falconfig.Name,
		Title: "FAL",
		Ready: ready,
		Setup: generation.Setup{Badge: "paid", Key: falconfig.APIKey, Prompt: "FAL API key", Link: falconfig.KeyLink},
		Images: &generation.Images{
			About:    "Many image models with edits, needs a FAL API key",
			Section:  falconfig.Key,
			Model:    falconfig.Model,
			Models:   imageModels,
			Default:  func(context.Context, *config.Config) string { return defaultPicture },
			Takes:    imageTakes,
			Generate: drawImage,
		},
		Videos: &generation.Videos{
			About:    "Many video models from text or an image, needs a FAL API key",
			Section:  falconfig.VideoKey,
			Model:    falconfig.VideoModel,
			Models:   videoModels,
			Default:  func(context.Context, *config.Config) string { return defaultFamily },
			Takes:    videoTakes,
			Generate: makeVideo,
		},
	}}
}
