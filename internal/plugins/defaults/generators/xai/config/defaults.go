package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	storage := func(key, what string) []config.Setting {
		return []config.Setting{
			{Key: key + StorageEnabled, Title: "xAI " + what + " storage", About: "Keep " + what + "s xAI makes in xAI storage for a reusable link. xAI may bill for stored files", Default: true},
			{Key: key + StoragePublic, Title: "xAI " + what + " link", About: "Give " + what + "s kept in xAI storage a public link", Default: true},
			{Key: key + StorageExpires, Title: "xAI " + what + " expiry", About: "Seconds xAI keeps stored " + what + "s, at most 30 days. Empty, never, permanent or 0 keeps them until you delete them", Default: ""},
		}
	}
	cfg.Register(config.Sections{{
		Key:    ImageKey,
		Parent: generationconfig.ImageKey,
		Title:  "xAI Grok Imagine images",
		About:  "Pictures Grok Imagine draws and edits, with the xAI key or the Grok sign-in connected in /providers",
		Settings: append([]config.Setting{
			{Key: ImageModel, Title: "xAI image model", About: "Model xAI draws with. Empty uses Image model, else xAI's default image model", Default: "", Env: []string{"XAI_IMAGE_MODEL"}},
			{Key: Resolution, Title: "xAI resolution", About: "Picture size xAI draws at", Default: "1k",
				Options: config.Options(config.Option{Value: "1k", Title: "1K", About: "About one megapixel"}, config.Option{Value: "2k", Title: "2K", About: "About four megapixels"})},
		}, storage(ImageKey, "picture")...),
	}})
	cfg.Register(config.Sections{{
		Key:      VideoKey,
		Parent:   generationconfig.VideoKey,
		Title:    "xAI Grok Imagine videos",
		About:    "Videos Grok Imagine makes, edits and extends, with the xAI key or the Grok sign-in connected in /providers",
		Settings: storage(VideoKey, "video"),
	}})
}
