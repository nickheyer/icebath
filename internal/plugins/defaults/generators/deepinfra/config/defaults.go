package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "DeepInfra",
		About:  "Open image models hosted by DeepInfra, with a DeepInfra account",
		Order:  70,
		Settings: []config.Setting{
			{Key: Model, Title: "Model", About: "The DeepInfra model that makes images.", Default: "", Zero: "The first model DeepInfra lists",
				Env: []string{"DEEPINFRA_IMAGE_MODEL"}, Picks: generationconfig.ImageModelOf(Name)},
			{Key: BaseURL, Title: "Base URL", About: "The address DeepInfra image requests go to.", Default: "", Zero: "The provider's address", Advanced: true},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    VideoKey,
		Parent: generationconfig.VideoKey,
		Title:  "DeepInfra",
		About:  "Open video models hosted by DeepInfra, with a DeepInfra account",
		Order:  40,
		Settings: []config.Setting{
			{Key: VideoModel, Title: "Model", About: "The DeepInfra model that makes videos.", Default: "", Zero: "The first model DeepInfra lists",
				Picks: generationconfig.VideoModelOf(Name)},
		},
	}})
}
