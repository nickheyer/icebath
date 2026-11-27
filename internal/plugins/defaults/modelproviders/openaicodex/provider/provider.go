package provider

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
	codexmodels "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/models"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := openaicodexconfig.BaseURL
	return []provider.Spec{{
		Name:               openaicodexconfig.Name,
		Native:             native,
		DisplayName:        openaicodexconfig.Title,
		Rank:               1,
		Description:        "GPT models through your ChatGPT plan, with no API key",
		BaseURL:            base,
		BaseURLEnv:         openaicodexconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthExternal,
		Aliases:            []string{"codex", "openai_codex", "chatgpt", "chatgpt-codex"},
		DefaultModel:       openaicodexconfig.DefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "openai",
		StaticModels:       codexmodels.Curated(),
		ModelMetadata:      map[string]map[string]any{"gpt-6-astra": {"limit": map[string]any{"context": float64(1_050_000), "output": float64(128_000)}, "modalities": map[string]any{"input": []any{"text", "image"}, "output": []any{"text"}}, "tool_call": true, "reasoning": true, "family": "gpt-6"}},
		Included:           true,
		ProbeAlways:        true,
		AccountUsage:       accountUsage,
		ResetUsage:         resetUsage,
		ResponsesIssuer:    "codex_backend",
		ReservedTools:      []string{"tool_search"},
		RequestHeaders:     identity,
		Finish:             finish,
		Settings: []config.Setting{
			{Key: openaicodexconfig.LoginFlow, Title: "Sign-in method", About: "How you sign in to ChatGPT for Codex.", Default: openaicodexconfig.DeviceCode,
				Options: config.Options(config.Option{Value: openaicodexconfig.DeviceCode, Title: "Device code", About: "Enter a code on another device"},
					config.Option{Value: openaicodexconfig.Browser, Title: "Browser", About: "Sign in through a browser window on this computer"})},
			{Key: openaicodexconfig.Daybreak, Title: "Security research access", About: "Ask Codex for security research access in your own sessions.",
				Help: "Only sessions in your own terminal ask, and only on a Codex plan that offers this access.", Default: false},
			{Key: openaicodexconfig.RefreshTimeout, Title: "Renewal time limit", About: "How long a renewal of the sign-in may take.", Default: 20 * time.Second,
				Env: []string{openaicodexconfig.RefreshTimeoutEnv}, Advanced: true},
		},
		Account: []config.Setting{
			{Key: openaicodexconfig.AccessToken, Title: "Access token", About: "Your ChatGPT sign-in token.", Default: "", Secret: true, Internal: true},
			{Key: openaicodexconfig.RefreshToken, Title: "Refresh token", About: "Your token that renews the sign-in before it expires.", Default: "", Secret: true,
				Internal: true},
			{Key: openaicodexconfig.LastRefresh, Title: "Last renewed", About: "When the sign-in tokens were last renewed.", Default: "", Internal: true},
		},
	}}
}
