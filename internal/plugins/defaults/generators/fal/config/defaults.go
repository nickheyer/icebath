package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "FAL.ai",
		About:  "FAL.ai's queue, which draws pictures and makes videos with FLUX, GPT Image, Nano Banana, Veo, Kling and more",
		Settings: []config.Setting{
			{Key: APIKey, Title: "FAL key", About: "FAL.ai API key for pictures and videos, from " + KeyLink, Default: "", Env: []string{"FAL_KEY"}, Secret: true},
			{Key: KeyID, Title: "FAL key id", About: "Id of a FAL key kept as an id and a secret, used with the FAL key secret when no FAL key is set", Default: "", Env: []string{"FAL_KEY_ID"}},
			{Key: KeySecret, Title: "FAL key secret", About: "Secret of a FAL key kept as an id and a secret", Default: "", Env: []string{"FAL_KEY_SECRET"}, Secret: true},
			{Key: RunHost, Title: "FAL host", About: "Host whose queue FAL calls go to, as queue.<host>. Empty uses " + QueueRun, Default: "", Env: []string{"FAL_RUN_HOST"}},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    VideoKey,
		Parent: generationconfig.VideoKey,
		Title:  "FAL video",
		About:  "Videos FAL makes",
		Settings: []config.Setting{
			{Key: VideoModel, Title: "FAL video model", About: "Model family FAL makes videos with, such as veo3.1 or kling-v3, or one of its endpoints. Empty uses Video model, else FAL's default", Default: "", Env: []string{"FAL_VIDEO_MODEL"}},
		},
	}})
}
