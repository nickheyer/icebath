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
		Description:        "MiniMax models, with an API key",
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
		Description:        "MiniMax models, with an API key, served from mainland China",
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
		Description:        "MiniMax models through your MiniMax account, with no API key",
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
			{Key: minimaxconfig.AccessToken, Title: "Access token", About: "Your MiniMax sign-in token.", Default: "", Secret: true, Internal: true},
			{Key: minimaxconfig.RefreshToken, Title: "Refresh token", About: "Your token that renews the sign-in before it expires.", Default: "", Secret: true,
				Internal: true},
			{Key: minimaxconfig.Expiry, Title: "Sign-in expiry", About: "When the access token expires.", Help: "A time in Unix milliseconds.", Default: 0,
				Internal: true},
			{Key: minimaxconfig.Portal, Title: "Portal", About: "The address where the sign-in was made and is renewed.", Default: "", Internal: true},
			{Key: minimaxconfig.Client, Title: "Client", About: "The client ID the sign-in was made with.", Default: "", Internal: true},
			{Key: minimaxconfig.Region, Title: "Region", About: "The MiniMax region the sign-in is for, global or cn.", Default: "", Internal: true},
			{Key: minimaxconfig.Endpoint, Title: "Model address", About: "The address of the models in the region of the sign-in.", Default: "", Internal: true},
		},
	}}
}
