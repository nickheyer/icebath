package provider

import (
	minimaxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               minimaxconfig.GlobalName,
		DisplayName:        minimaxconfig.GlobalTitle,
		BaseURL:            minimaxconfig.GlobalBaseURL,
		BaseURLEnv:         minimaxconfig.GlobalBaseURLEnv,
		ModelsURL:          minimaxconfig.GlobalModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{minimaxconfig.GlobalAPIKeyEnv},
		Aliases:            []string{"mini-max"},
		DefaultModel:       minimaxconfig.GlobalDefaultModel,
		DefaultAuxModel:    "MiniMax-M3",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "minimax",
	}, {
		Name:               minimaxconfig.CNName,
		DisplayName:        minimaxconfig.CNTitle,
		BaseURL:            minimaxconfig.CNBaseURL,
		BaseURLEnv:         minimaxconfig.CNBaseURLEnv,
		ModelsURL:          minimaxconfig.CNModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{minimaxconfig.CNAPIKeyEnv},
		Aliases:            []string{"minimax-china", "minimax_cn"},
		DefaultModel:       minimaxconfig.CNDefaultModel,
		DefaultAuxModel:    "MiniMax-M3",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "minimax-cn",
	}, {
		Name:               minimaxconfig.OAuthName,
		DisplayName:        minimaxconfig.OAuthTitle,
		Description:        "Browser sign-in, no API key",
		SignupURL:          "https://api.minimax.io/",
		BaseURL:            minimaxconfig.OAuthBaseURL,
		ModelsURL:          minimaxconfig.OAuthModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthMiniMax,
		Aliases:            []string{"minimax_oauth", "minimax-portal", "minimax-global", "minimax-oauth-io"},
		DefaultModel:       minimaxconfig.OAuthDefaultModel,
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
