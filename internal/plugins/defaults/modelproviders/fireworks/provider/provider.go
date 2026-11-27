package provider

import (
	fireworksconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := fireworksconfig.BaseURL
	return []provider.Spec{{
		Name:               fireworksconfig.Name,
		Shape:              shape,
		Carriers:           []string{"reasoning_content"},
		DisplayName:        fireworksconfig.Title,
		Description:        "OpenAI-compatible model API",
		SignupURL:          "https://app.fireworks.ai/settings/users/api-keys",
		BaseURL:            base,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{fireworksconfig.APIKeyEnv},
		Aliases:            []string{"fireworks-ai", "fw"},
		DefaultModel:       fireworksconfig.DefaultModel,
		FallbackModels:     []string{"accounts/fireworks/models/kimi-k2p6", "accounts/fireworks/models/glm-5p2", "accounts/fireworks/models/kimi-k2p7-code"},
		Headers:            map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent", "User-Agent": provider.UserAgent("IcebathAgent")},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "fireworks-ai",
		ModelsDevPreferred: true,
		Prices:             prices,
		Billing:            []provider.Billing{{Label: "Fireworks AI", URL: "https://fireworks.ai/account/billing", Hosts: []string{"fireworks.ai"}}},
		DefaultAuxModel:    "accounts/fireworks/models/glm-5p2",
	}}
}
