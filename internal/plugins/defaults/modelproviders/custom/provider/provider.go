package provider

import (
	"cmp"

	"github.com/nickheyer/icebath/internal/config"
	customconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

// Endpoint of your own: a server that speaks one of the APIs icebath does, at the base URL it is given, its API key optional
func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:        customconfig.Name,
		DisplayName: customconfig.Title,
		Description: "Server of your own that speaks a supported API",
		Mode:        provider.ModeChatCompletions,
		Auth:        provider.AuthNone,
		Local:       true,
		Settings: []config.Setting{
			{Key: customconfig.APIMode, Title: "API", About: "API the server speaks", Default: string(provider.ModeChatCompletions), Options: config.Options(
				config.Option{Value: string(provider.ModeChatCompletions), Title: "OpenAI chat completions"},
				config.Option{Value: string(provider.ModeAnthropicMessages), Title: "Anthropic messages"},
				config.Option{Value: string(provider.ModeCodexResponses), Title: "OpenAI responses"},
			)},
			{Key: customconfig.KeyEnv, Title: "API key variable", About: "Environment variable to read the API key from when the account keeps none", Default: ""},
		},
		Resolve: func(spec provider.Spec, settings config.View) provider.Spec {
			spec.Mode = provider.APIMode(cmp.Or(settings.GetString(customconfig.APIMode), string(provider.ModeChatCompletions)))
			if variable := settings.GetString(customconfig.KeyEnv); variable != "" {
				spec.KeyEnv = []string{variable}
			}
			return spec
		},
	}}
}
