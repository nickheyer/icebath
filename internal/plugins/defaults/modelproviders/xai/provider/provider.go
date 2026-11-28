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
		Description:        "Grok models, with an xAI API key paid by use",
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
		SignIn:             true,
		ReservedTools:      []string{"tool_search"},
		RejectionHint:      rejectionHint,
		ResponsesIssuer:    "xai_responses",
		Efforts:            efforts,
		Native:             provider.Unprefixed,
		DisplayName:        xaiconfig.OAuthTitle,
		Description:        "Grok models through your SuperGrok or X Premium+ plan, with no key",
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
		Settings: []config.Setting{{Key: xaiconfig.RefreshTimeout, Title: "Renewal time limit", About: "How long a renewal of the Grok sign-in may take.",
			Default: 20 * time.Second, Env: []string{xaiconfig.RefreshTimeoutEnv}, Advanced: true}},
		Account: []config.Setting{
			{Key: xaiconfig.AccessToken, Title: "Access token", About: "Your Grok sign-in token.", Default: "", Secret: true, Internal: true},
			{Key: xaiconfig.RefreshToken, Title: "Refresh token", About: "Your token that renews the sign-in before it expires.", Default: "", Secret: true,
				Internal: true},
			{Key: xaiconfig.IDToken, Title: "ID token", About: "Your token that tells who signed in.", Default: "", Secret: true, Internal: true},
			{Key: xaiconfig.TokenEndpoint, Title: "Renewal address", About: "The address that renewals of the sign-in are sent to.", Default: "", Internal: true},
			{Key: xaiconfig.LastRefresh, Title: "Last renewed", About: "When the sign-in tokens were last renewed.", Default: "", Internal: true},
		},
	}}
}
