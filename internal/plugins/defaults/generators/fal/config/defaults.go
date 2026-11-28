package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "FAL",
		About:  "Many image and video models, with a FAL API key",
		Order:  30,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your FAL API key, which makes its images and its videos.", Help: "Get one at " + KeyLink + ".",
				Default: "", Env: []string{"FAL_KEY"}, Secret: true},
			{Key: Model, Title: "Model", About: "The FAL model that makes images.", Default: "", Zero: "FLUX 2 Klein 9B",
				Picks: generationconfig.ImageModelOf(Name)},
			{Key: KeyID, Title: "Key ID", About: "The ID of a FAL key kept as an ID and a secret.", Help: "Used with Key secret when no API key is set.",
				Default: "", Env: []string{"FAL_KEY_ID"}, Advanced: true},
			{Key: KeySecret, Title: "Key secret", About: "Your FAL key secret, used with Key ID.", Default: "", Env: []string{"FAL_KEY_SECRET"}, Secret: true,
				Advanced: true},
			{Key: RunHost, Title: "Queue host", About: "The host whose queue FAL requests go to, as queue.<host>.",
				Help: "FAL's own queue is at " + QueueRun + ".", Default: "", Zero: "FAL's own queue", Env: []string{"FAL_RUN_HOST"}, Advanced: true},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    VideoKey,
		Parent: generationconfig.VideoKey,
		Title:  "FAL",
		About:  "Many video models, with a FAL API key",
		Order:  10,
		Settings: []config.Setting{
			{Key: VideoModel, Title: "Model", About: "The FAL model family that makes videos.", Help: "The full name of a FAL video model works too.",
				Default: "", Zero: "Pixverse v6", Env: []string{"FAL_VIDEO_MODEL"}, Picks: generationconfig.VideoModelOf(Name)},
		},
	}})
}
