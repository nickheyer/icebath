package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "Meta AI",
		About:  "Images from Meta, with the Meta account in Providers",
		Order:  80,
		Settings: []config.Setting{
			{Key: Model, Title: "Model", About: "The Meta model that makes images.", Default: "", Zero: "Muse Image 1.0", Env: []string{"META_IMAGE_MODEL"}},
		},
	}})
}
