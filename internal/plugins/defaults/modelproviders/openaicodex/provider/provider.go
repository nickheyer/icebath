package provider

import (
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
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
		DisplayName:        openaicodexconfig.Title,
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
		ModelsDevID:        openaicodexconfig.Name,
	}}
}
