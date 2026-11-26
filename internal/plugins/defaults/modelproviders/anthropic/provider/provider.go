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
		DisplayName:        anthropicconfig.Title,
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
		Settings: []config.Setting{
			{Key: anthropicconfig.AdoptExternalLogins, Title: "Use Claude Code login", About: "Use Claude Code's login if one is on this machine", Default: true},
		},
		Account: []config.Setting{
			providerconfig.APIKey(),
			{Key: anthropicconfig.Token, Title: "Sign-in token", About: "Claude sign-in token", Default: "", Secret: true, Internal: true},
			{Key: anthropicconfig.OAuthToken, Title: "Setup token", About: "Token from claude setup-token", Default: "", Secret: true, Internal: true},
			{Key: anthropicconfig.RefreshToken, Title: "Refresh token", About: "Renews the sign-in token before it expires", Default: "", Secret: true, Internal: true},
			{Key: anthropicconfig.TokenExpires, Title: "Sign-in expiry", About: "When the sign-in token expires, in Unix milliseconds", Default: 0, Internal: true},
		},
	}}
}
