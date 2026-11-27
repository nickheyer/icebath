package provider

import (
	"github.com/nickheyer/icebath/internal/config"
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
		Caching:            caching,
		DottedModels:       true,
		Shape:              shape,
		Carriers:           []string{"reasoning_content", "reasoning_details"},
		Native:             provider.Unprefixed,
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
		StaticModels:       []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5", "MiniMax-M2.1", "MiniMax-M2"},
		MetadataHostnames:  []string{"api.minimax"},
		Prices:             prices,
	}, {
		Name:               minimaxconfig.CNName,
		Caching:            caching,
		DottedModels:       true,
		Shape:              shape,
		Carriers:           []string{"reasoning_content", "reasoning_details"},
		Native:             provider.Unprefixed,
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
		StaticModels:       []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5", "MiniMax-M2.1", "MiniMax-M2"},
		Prices:             prices,
	}, {
		Name:               minimaxconfig.OAuthName,
		Caching:            caching,
		Shape:              shape,
		Carriers:           []string{"reasoning_content", "reasoning_details"},
		Native:             provider.Unprefixed,
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
		StaticModels:       []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed"},
		PortalBaseURL:      minimaxconfig.OAuthPortal,
		ClientID:           minimaxconfig.OAuthClientID,
		Scope:              minimaxconfig.OAuthScope,
		Extra:              map[string]any{"region": "global", "cn_portal_base_url": minimaxconfig.OAuthCNPortal, "cn_inference_base_url": minimaxconfig.OAuthCNBaseURL},
		Account: []config.Setting{
			{Key: minimaxconfig.AccessToken, Title: "Access token", About: "Token of the MiniMax sign-in", Default: "", Secret: true, Internal: true},
			{Key: minimaxconfig.RefreshToken, Title: "Refresh token", About: "Renews the access token before it expires", Default: "", Secret: true, Internal: true},
			{Key: minimaxconfig.Expiry, Title: "Sign-in expiry", About: "When the access token expires, in Unix milliseconds", Default: 0, Internal: true},
			{Key: minimaxconfig.Portal, Title: "Portal", About: "Where the sign-in was made and is renewed", Default: "", Internal: true},
			{Key: minimaxconfig.Client, Title: "Client", About: "Client the sign-in was made with", Default: "", Internal: true},
			{Key: minimaxconfig.Region, Title: "Region", About: "global or cn", Default: "", Internal: true},
			{Key: minimaxconfig.Endpoint, Title: "Inference address", About: "Address of the region's API the sign-in is for", Default: "", Internal: true},
		},
	}}
}
