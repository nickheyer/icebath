package provider

import (
	"cmp"

	"github.com/nickheyer/icebath/internal/config"
	customconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
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
		Description: "A model server you run or rent, at an address you give",
		Mode:        provider.ModeChatCompletions,
		Auth:        provider.AuthNone,
		Local:       true,
		Settings: []config.Setting{
			{Key: customconfig.KeyEnv, Title: "Key variable", About: "The environment variable that holds the API key.",
				Help: "Read when the account keeps no API key.", Default: "", Advanced: true},
			{Key: providerconfig.CatalogProvider, Title: "Model details from", About: "The provider whose details, such as limits and prices, these models take.",
				Help: "A kind of provider, or a provider's name in the models.dev registry.", Default: "", Advanced: true},
			{Key: customconfig.OllamaNumCtx, Title: "Ollama context size", About: "The context size an Ollama server is asked for on every request.",
				Default: 0, Zero: "The server's own", Counts: "tokens", Advanced: true},
		},
		Billing: []provider.Billing{
			{Label: "Groq", URL: "https://console.groq.com/settings/billing", Hosts: []string{"api.groq.com"}},
			{Label: "Mistral", URL: "https://console.mistral.ai/billing", Hosts: []string{"api.mistral.ai"}},
			{Label: "Together AI", URL: "https://api.together.ai/settings/billing", Hosts: []string{"api.together.ai", "api.together.xyz"}},
			{Label: "Perplexity", URL: "https://www.perplexity.ai/settings/api", Hosts: []string{"perplexity.ai"}},
			{Label: "Cohere", URL: "https://dashboard.cohere.com/billing", Hosts: []string{"api.cohere.com", "api.cohere.ai"}},
		},
		Resolve: func(spec provider.Spec, settings config.View) provider.Spec {
			if !provider.KnownMode(settings.GetString(providerconfig.APIMode)) {
				spec.Mode = cmp.Or(provider.ModeForURL(spec.BaseURL), provider.ModeChatCompletions)
			}
			if provider.BaseHostMatches(spec.BaseURL, "api.perplexity.ai") {
				spec.ReservedTools = perplexityReserved
			}
			if variable := settings.GetString(customconfig.KeyEnv); variable != "" {
				spec.KeyEnv = []string{variable}
			}
			spec.Carriers = carriers(spec.BaseURL)
			spec.ShortToolIDs = provider.BaseHostMatches(spec.BaseURL, "mistral.ai")
			return spec
		},
		DefaultReasoning: defaultReasoning,
		Shape:            shape,
		Efforts:          efforts,
		Carriers:         carriers(""),
		Native:           func(_ provider.Spec, model string) string { return provider.StripOwn(model, customconfig.Name) },
	}}
}
