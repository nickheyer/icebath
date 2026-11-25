package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	minimaxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	return []provider.Profile{{
		Name:               minimaxconfig.GlobalName,
		DisplayName:        "MiniMax",
		BaseURL:            p.config.GetString(minimaxconfig.GlobalBaseURL),
		BaseURLEnv:         minimaxconfig.GlobalBaseURLEnv,
		ModelsURL:          minimaxconfig.GlobalModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{minimaxconfig.GlobalAPIKeyEnv},
		Aliases:            []string{"mini-max"},
		DefaultModel:       p.config.GetString(minimaxconfig.GlobalDefaultModel),
		DefaultAuxModel:    "MiniMax-M3",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "minimax",
	}, {
		Name:               minimaxconfig.CNName,
		DisplayName:        "MiniMax (China)",
		BaseURL:            p.config.GetString(minimaxconfig.CNBaseURL),
		BaseURLEnv:         minimaxconfig.CNBaseURLEnv,
		ModelsURL:          minimaxconfig.CNModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{minimaxconfig.CNAPIKeyEnv},
		Aliases:            []string{"minimax-china", "minimax_cn"},
		DefaultModel:       p.config.GetString(minimaxconfig.CNDefaultModel),
		DefaultAuxModel:    "MiniMax-M3",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "minimax-cn",
	}, {
		Name:               minimaxconfig.OAuthName,
		DisplayName:        "MiniMax (OAuth)",
		Description:        "MiniMax via OAuth browser flow - no API key required",
		SignupURL:          "https://api.minimax.io/",
		BaseURL:            p.config.GetString(minimaxconfig.OAuthBaseURL),
		ModelsURL:          minimaxconfig.OAuthModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthMiniMax,
		Aliases:            []string{"minimax_oauth", "minimax-portal", "minimax-global", "minimax-oauth-io"},
		DefaultModel:       p.config.GetString(minimaxconfig.OAuthDefaultModel),
		DefaultAuxModel:    "MiniMax-M2.7",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "minimax",
		PortalBaseURL:      "https://api.minimax.io",
		ClientID:           "78257093-7e40-4613-99e0-527b14b39113",
		Scope:              "group_id profile model.completion",
		Extra:              map[string]any{"region": "global", "cn_portal_base_url": "https://api.minimaxi.com", "cn_inference_base_url": "https://api.minimaxi.com/anthropic"},
	}}
}
