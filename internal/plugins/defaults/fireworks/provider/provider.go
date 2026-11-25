package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	fireworksconfig "github.com/nickheyer/icebath/internal/plugins/defaults/fireworks/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(fireworksconfig.BaseURL)
	return []provider.Profile{{
		Name:               fireworksconfig.Name,
		DisplayName:        "Fireworks AI",
		Description:        "Fireworks AI — OpenAI-compatible direct model API",
		SignupURL:          "https://app.fireworks.ai/settings/users/api-keys",
		BaseURL:            base,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{fireworksconfig.APIKeyEnv},
		Aliases:            []string{"fireworks-ai", "fw"},
		DefaultModel:       p.config.GetString(fireworksconfig.DefaultModel),
		FallbackModels:     []string{"accounts/fireworks/models/kimi-k2p6", "accounts/fireworks/models/glm-5p2", "accounts/fireworks/models/kimi-k2p7-code"},
		Headers:            map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent", "User-Agent": "IcebathAgent/dev"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        fireworksconfig.Name,
		DefaultAuxModel:    "accounts/fireworks/models/glm-5p2",
	}}
}
