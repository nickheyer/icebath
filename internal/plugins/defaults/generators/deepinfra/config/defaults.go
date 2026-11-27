package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "DeepInfra images",
		About:  "Pictures DeepInfra draws through its OpenAI compatible images API, from the image models of its live catalog, with the DeepInfra key connected in /providers",
		Settings: []config.Setting{
			{Key: Model, Title: "DeepInfra image model", About: "Model DeepInfra draws with. Empty takes the first of DeepInfra's catalog", Default: "", Env: []string{"DEEPINFRA_IMAGE_MODEL"}},
			{Key: BaseURL, Title: "DeepInfra image address", About: "Custom endpoint for DeepInfra image requests. Empty takes the DeepInfra provider's", Default: ""},
		},
	}})
}
