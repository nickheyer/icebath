package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
	"github.com/nickheyer/icebath/internal/provider"
)

func Register(cfg *config.Config, providers *provider.Registry) {
	endpoints := func() []config.Option {
		return append([]config.Option{{Value: "", Title: "OpenAI", About: "The OpenAI endpoint, or the one OPENAI_BASE_URL names"}}, providers.Options(cfg)()...)
	}
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "OpenAI images",
		About:  "Pictures GPT Image 2 and GPT Image 2.5 draw and edit through the OpenAI images API, with the OpenAI key or a provider of the profile",
		Settings: []config.Setting{
			{Key: Model, Title: "OpenAI image model", About: "Model OpenAI draws with, a tier name such as gpt-image-2-high or any model id. Empty uses Image model, else GPT Image 2 at medium quality",
				Default: "", Env: []string{"OPENAI_IMAGE_MODEL"}},
			{Key: Provider, Title: "OpenAI image endpoint", About: "Provider of the profile whose address and key OpenAI image requests take. Empty uses the OpenAI endpoint",
				Default: "", Options: endpoints},
			{Key: BaseURL, Title: "OpenAI image address", About: "Base URL OpenAI image requests go to. Wins over OpenAI image endpoint; empty takes the endpoint's address, else OPENAI_BASE_URL",
				Default: ""},
			{Key: KeyEnv, Title: "OpenAI image key", About: "Name of the variable holding the key OpenAI image requests send. Empty takes the endpoint's key, else OPENAI_API_KEY",
				Default: ""},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    CodexKey,
		Parent: generationconfig.ImageKey,
		Title:  "OpenAI images via Codex",
		About:  "Pictures gpt-image-2 draws through your ChatGPT or Codex sign-in, no API key needed",
		Settings: []config.Setting{
			{Key: CodexModel, Title: "Codex image tier", About: "Quality tier of pictures drawn through your Codex sign-in: gpt-image-2-low, gpt-image-2-medium or gpt-image-2-high. Empty uses Image model, else medium",
				Default: "", Env: []string{"OPENAI_IMAGE_MODEL"}},
		},
	}})
}
