package provider

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := xaiconfig.XAIBaseURL
	oauthBase := xaiconfig.OAuthBaseURL
	return []provider.Spec{{
		Name:               xaiconfig.XAIName,
		ReservedTools:      []string{"tool_search"},
		RejectionHint:      rejectionHint,
		ResponsesIssuer:    "xai_responses",
		Efforts:            efforts,
		Native:             provider.Unprefixed,
		DisplayName:        xaiconfig.XAITitle,
		BaseURL:            base,
		BaseURLEnv:         xaiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{xaiconfig.APIKeyEnv},
		Aliases:            []string{"grok", "x-ai", "x.ai"},
		DefaultModel:       xaiconfig.XAIDefaultModel,
		Headers:            map[string]string{"User-Agent": provider.UserAgent("Icebath-Agent")},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "xai",
		StaticModels:       []string{"grok-4.6", "grok-composer-2.5-fast", "grok-build-0.1", "grok-4.5", "grok-4.3", "grok-4.20-0309-reasoning", "grok-4.20-0309-non-reasoning", "grok-4.20-multi-agent-0309"},
		ModelsDevPreferred: true,
		ListPrices:         "x.ai",
		Billing:            []provider.Billing{{Label: "xAI", URL: "https://console.x.ai/team/default/billing"}},
	}, {
		Name:               xaiconfig.OAuthName,
		ReservedTools:      []string{"tool_search"},
		RejectionHint:      rejectionHint,
		ResponsesIssuer:    "xai_responses",
		Efforts:            efforts,
		Native:             provider.Unprefixed,
		DisplayName:        xaiconfig.OAuthTitle,
		BaseURL:            oauthBase,
		BaseURLEnv:         xaiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(oauthBase),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthExternal,
		Aliases:            []string{"grok-oauth", "x-ai-oauth", "xai-grok-oauth"},
		DefaultModel:       xaiconfig.OAuthDefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "xai",
		StaticModels:       []string{"grok-4.6", "grok-composer-2.5-fast", "grok-build-0.1", "grok-4.5", "grok-4.3", "grok-4.20-0309-reasoning", "grok-4.20-0309-non-reasoning", "grok-4.20-multi-agent-0309"},
		ModelsDevPreferred: true,
		Billing:            []provider.Billing{{Label: "xAI", URL: "https://console.x.ai/team/default/billing"}},
		Settings: []config.Setting{{Key: xaiconfig.RefreshTimeout, Title: "Refresh timeout", About: "Seconds a renewal of the Grok sign-in may take",
			Default: 20 * time.Second, Env: []string{xaiconfig.RefreshTimeoutEnv}}},
		Account: []config.Setting{
			{Key: xaiconfig.AccessToken, Title: "Access token", About: "Token of the Grok sign-in", Default: "", Secret: true, Internal: true},
			{Key: xaiconfig.RefreshToken, Title: "Refresh token", About: "Renews the access token before it expires", Default: "", Secret: true, Internal: true},
			{Key: xaiconfig.IDToken, Title: "ID token", About: "Who the sign-in is of", Default: "", Secret: true, Internal: true},
			{Key: xaiconfig.TokenEndpoint, Title: "Token endpoint", About: "Where renewals of the sign-in are sent", Default: "", Internal: true},
			{Key: xaiconfig.LastRefresh, Title: "Last renewed", About: "When the tokens were last renewed", Default: "", Internal: true},
		},
	}}
}
