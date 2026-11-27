package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "Meta AI images",
		About:  "Pictures Muse Image draws through the Meta Model API, with the Meta key connected in /providers",
		Settings: []config.Setting{
			{Key: Model, Title: "Meta AI model", About: "Model Meta AI draws with. Empty uses Image model, else " + DefaultModel, Default: "", Env: []string{"META_IMAGE_MODEL"}},
		},
	}})
}
