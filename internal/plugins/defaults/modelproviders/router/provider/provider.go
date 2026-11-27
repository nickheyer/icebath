package provider

import (
	routerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := routerconfig.BaseURL
	return []provider.Spec{{
		Name:               routerconfig.Name,
		DisplayName:        routerconfig.Title,
		Description:        "Picks the cheapest model good enough for each request, with an API key",
		SignupURL:          "https://app.router.com/keys",
		BaseURL:            base,
		BaseURLEnv:         routerconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{routerconfig.APIKeyEnv, routerconfig.RouterAPIKeyEnv},
		Aliases:            []string{"ramp-router", "ramp", "router.com"},
		DefaultModel:       routerconfig.DefaultModel,
		Headers:            map[string]string{"User-Agent": provider.UserAgent("Icebath-Agent")},
		ModelListing:       true,
		HealthCheck:        true,
		Vision:             true,
		VisionToolMessages: true,
		DefaultAuxModel:    "gpt-5.4-mini",
	}}
}
