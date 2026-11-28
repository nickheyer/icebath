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
		About:  "Stylized images and edits, with a Krea API key",
		Order:  60,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Krea API key.", Help: "Get one at " + KeyLink + ".", Default: "", Env: []string{"KREA_API_KEY"}, Secret: true},
			{Key: Model, Title: "Model", About: "The Krea model that makes images.", Default: "", Zero: "Krea 2 Medium", Env: []string{"KREA_IMAGE_MODEL"},
				Picks: generationconfig.ImageModelOf(Name)},
			{Key: Creativity, Title: "Creativity", About: "How freely Krea 2 models follow the description of an image.", Default: "medium",
				Options: config.Options(
					config.Option{Value: "raw", Title: "Raw", About: "The description as written"},
					config.Option{Value: "low", Title: "Low", About: "Close to the description"},
					config.Option{Value: "medium", Title: "Medium", About: "Some freedom"},
					config.Option{Value: "high", Title: "High", About: "The most freedom"},
				)},
			{Key: Upscale, Title: "Enlarge images", About: "Whether Krea enlarges each image it makes.", Default: "",
				Options: config.Options(
					config.Option{Value: "", Title: "Krea's default", About: "No enlarging unless the agent asks for it"},
					config.Option{Value: "true", Title: "On", About: "Enlarge every image unless the agent asks not to"},
					config.Option{Value: "false", Title: "Off", About: "No enlarging unless the agent asks for it"},
				)},
		},
	}})
}
