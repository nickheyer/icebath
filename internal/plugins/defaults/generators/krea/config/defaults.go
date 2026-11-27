package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "Krea",
		About:  "Pictures Krea 3 draws and edits, and Krea 2 Medium, Large and Medium Turbo draw with style references and moodboards",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Krea key", About: "Krea API key, from " + KeyLink, Default: "", Env: []string{"KREA_API_KEY"}, Secret: true},
			{Key: Model, Title: "Krea model", About: "Model Krea draws with: krea-3, krea-2-medium, krea-2-large or krea-2-medium-turbo. Empty uses Image model, else Krea 2 Medium",
				Default: "", Env: []string{"KREA_IMAGE_MODEL"}},
			{Key: Creativity, Title: "Krea creativity", About: "How freely Krea 2 reads your prompt", Default: "medium",
				Options: config.Options(config.Option{Value: "raw", Title: "Raw", About: "Your prompt as written"}, config.Option{Value: "low", Title: "Low"},
					config.Option{Value: "medium", Title: "Medium"}, config.Option{Value: "high", Title: "High", About: "The most freedom"})},
			{Key: Upscale, Title: "Krea upscale", About: "Have Krea enlarge each picture it draws", Default: "",
				Options: config.Options(config.Option{Value: "", Title: "Model's default"}, config.Option{Value: "true", Title: "On"},
					config.Option{Value: "false", Title: "Off"})},
		},
	}})
}
