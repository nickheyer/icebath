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
		Description: "Server of your own that speaks a supported API",
		Mode:        provider.ModeChatCompletions,
		Auth:        provider.AuthNone,
		Local:       true,
		Settings: []config.Setting{
			{Key: customconfig.KeyEnv, Title: "API key variable", About: "Environment variable to read the API key from when the account keeps none", Default: ""},
			{Key: providerconfig.CatalogProvider, Title: "Model details from", About: "Kind of provider, or provider of the models.dev registry, " +
				"whose details such as limits and prices its models take", Default: ""},
			{Key: customconfig.OllamaNumCtx, Title: "Ollama context", About: "Context size an Ollama server is asked for on every request, in tokens; " +
				"0 uses the size the server reports", Default: 0},
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
