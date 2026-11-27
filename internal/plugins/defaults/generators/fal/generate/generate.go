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
		Title: "FAL.ai",
		Ready: ready,
		Setup: generation.Setup{Badge: "paid", Key: falconfig.APIKey, Prompt: "FAL API key", Link: falconfig.KeyLink},
		Images: &generation.Images{
			About:    "Pick from flux-2-klein, flux-2-pro, gpt-image, nano-banana-2, nano-banana-pro, etc. — text-to-image & image editing",
			Settings: []string{falconfig.APIKey, falconfig.KeyID, falconfig.KeySecret, falconfig.RunHost},
			Models:   imageModels,
			Default:  func(context.Context, *config.Config) string { return defaultPicture },
			Takes:    imageTakes,
			Generate: drawImage,
		},
		Videos: &generation.Videos{
			About: "LTX 2.3/2.5, Pixverse, Seedance 2.0/2.5/Mini, Veo 3.1, MiniMax H3, FLUX 3, Kling 3.0/4K/O3, Wan 3.0, Happy Horse, Grok Imagine, " +
				"Gemini Omni — text-to-video & image-to-video",
			Settings: []string{falconfig.VideoModel, falconfig.APIKey},
			Model:    falconfig.VideoModel,
			Models:   videoModels,
			Default:  func(context.Context, *config.Config) string { return defaultFamily },
			Takes:    videoTakes,
			Generate: makeVideo,
		},
	}}
}
