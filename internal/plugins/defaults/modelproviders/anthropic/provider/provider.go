package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := anthropicconfig.BaseURL
	return []provider.Spec{{
		Name:               anthropicconfig.Name,
		SignIn:             true,
		Native:             native,
		DisplayName:        anthropicconfig.Title,
		Rank:               3,
		Description:        "Claude models, with an API key or your Claude subscription",
		BaseURL:            base,
		BaseURLEnv:         anthropicconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{anthropicconfig.APIKeyEnv, anthropicconfig.TokenEnv, anthropicconfig.OAuthTokenEnv},
		Aliases:            []string{"claude", "claude-oauth", "claude-code"},
		DefaultModel:       anthropicconfig.DefaultModel,
		DefaultAuxModel:    "claude-haiku-4-5-20251001",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        anthropicconfig.Name,
		StaticModels:       []string{"claude-fable-5.1", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-4-6", "claude-opus-4-5-20251101", "claude-sonnet-4-5-20250929", "claude-opus-4-20250514", "claude-sonnet-4-20250514", "claude-haiku-4-5-20251001"},
		Prices:             prices,
		TierPrices:         map[string]map[string]provider.Price{"fast": fast},
		ListPrices:         "anthropic.com",
		Billing:            []provider.Billing{{Label: "Anthropic", URL: "https://console.anthropic.com/settings/billing", Plan: "https://claude.ai/settings/usage"}},
		AccountUsage:       accountUsage,
		Account: []config.Setting{
			providerconfig.APIKey(),
			{Key: anthropicconfig.Token, Title: "Sign-in token", About: "Your Claude sign-in token.", Default: "", Secret: true, Internal: true},
			{Key: anthropicconfig.OAuthToken, Title: "Setup token", About: "Your token from claude setup-token.", Default: "", Secret: true, Internal: true},
			{Key: anthropicconfig.RefreshToken, Title: "Refresh token", About: "Your token that renews the sign-in before it expires.", Default: "", Secret: true,
				Internal: true},
			{Key: anthropicconfig.TokenExpires, Title: "Sign-in expiry", About: "When the sign-in token expires.", Help: "A time in Unix milliseconds.", Default: 0,
				Internal: true},
		},
	}}
}
