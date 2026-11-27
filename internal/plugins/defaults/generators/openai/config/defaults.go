package config

import (
	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
	"github.com/nickheyer/icebath/internal/provider"
)

func Register(cfg *config.Config, providers *provider.Registry) {
	endpoints := func() []config.Option {
		return append([]config.Option{{Value: "", Title: "OpenAI", About: "Your OpenAI account in Providers, or OPENAI_BASE_URL when it is set"}},
			providers.Options(cfg)()...)
	}
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "OpenAI",
		About:  "Images and edits from OpenAI, with an OpenAI API key",
		Order:  10,
		Settings: []config.Setting{
			{Key: Model, Title: "Model", About: "The OpenAI model or quality tier that makes images.",
				Help: "A tier such as gpt-image-2-high, or any model ID. When neither model is set, GPT Image 2 at medium quality is used.", Default: "",
				Zero: "The general model", Env: []string{"OPENAI_IMAGE_MODEL"}},
			{Key: Provider, Title: "Provider", About: "Which provider's address and API key image requests use.", Default: "", Options: endpoints,
				Advanced: true},
			{Key: BaseURL, Title: "Base URL", About: "The address image requests go to.", Help: "This wins over Provider.", Default: "",
				Zero: "The provider's address", Advanced: true},
			{Key: KeyEnv, Title: "Key variable", About: "The environment variable that holds the API key.", Default: "", Zero: "The provider's key",
				Advanced: true},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    CodexKey,
		Parent: generationconfig.ImageKey,
		Title:  "OpenAI via ChatGPT",
		About:  "Images from OpenAI with your ChatGPT sign-in, no API key",
		Order:  20,
		Settings: []config.Setting{
			{Key: CodexModel, Title: "Model", About: "The quality tier of images made with your ChatGPT sign-in.",
				Help: "One of gpt-image-2-low, gpt-image-2-medium or gpt-image-2-high. When neither model is set, medium is used.", Default: "",
				Zero: "The general model", Env: []string{"OPENAI_IMAGE_MODEL"}},
		},
	}})
}
