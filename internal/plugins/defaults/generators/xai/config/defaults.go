package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	// Storage settings of images or videos, the noun being the plural word for what is kept
	storage := func(key, noun string) []config.Setting {
		return []config.Setting{
			{Key: key + StorageEnabled, Title: "Keep in xAI storage", About: "Keep the " + noun + " xAI makes in xAI storage for a link you can reuse.",
				Help: "xAI may bill for stored files.", Default: true},
			{Key: key + StoragePublic, Title: "Public link", About: "Give the " + noun + " kept in xAI storage a public link.", Default: true},
			{Key: key + StorageExpires, Title: "Keep for", About: "How long xAI keeps stored " + noun + ".",
				Help:    "A number of seconds up to 30 days, such as 86400 for one day. Typing never or permanent keeps them until you delete them.",
				Default: "", Zero: "Until you delete"},
		}
	}
	cfg.Register(config.Sections{{
		Key:    ImageKey,
		Parent: generationconfig.ImageKey,
		Title:  "xAI",
		About:  "Grok images and edits, with an xAI API key or sign-in",
		Order:  50,
		Settings: append([]config.Setting{
			{Key: ImageModel, Title: "Model", About: "The xAI model that makes images.", Help: "When neither model is set, xAI's default image model is used.",
				Default: "", Zero: "The general model", Env: []string{"XAI_IMAGE_MODEL"}},
			{Key: Resolution, Title: "Resolution", About: "Which size xAI makes images at.", Default: "1k",
				Options: config.Options(config.Option{Value: "1k", Title: "1K", About: "About one megapixel"}, config.Option{Value: "2k", Title: "2K",
					About: "About four megapixels"})},
		}, storage(ImageKey, "images")...),
	}})
	cfg.Register(config.Sections{{
		Key:      VideoKey,
		Parent:   generationconfig.VideoKey,
		Title:    "xAI",
		About:    "Grok videos you can edit and extend, with an xAI API key",
		Order:    20,
		Settings: storage(VideoKey, "videos"),
	}})
}
