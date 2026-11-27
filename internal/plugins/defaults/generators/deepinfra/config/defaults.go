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
		About:  "Open image models hosted by DeepInfra, with an API key",
		Order:  70,
		Settings: []config.Setting{
			{Key: Model, Title: "Model", About: "The DeepInfra model that makes images.", Help: "When neither model is set, the first model DeepInfra lists is used.",
				Default: "", Zero: "The general model", Env: []string{"DEEPINFRA_IMAGE_MODEL"}},
			{Key: BaseURL, Title: "Base URL", About: "The address DeepInfra image requests go to.", Default: "", Zero: "The provider's address", Advanced: true},
		},
	}})
}
