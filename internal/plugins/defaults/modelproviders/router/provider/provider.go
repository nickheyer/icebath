package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	routerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(routerconfig.BaseURL)
	return []provider.Profile{{
		Name:               routerconfig.Name,
		DisplayName:        "Ramp Router",
		Description:        "Ramp Router (router.com) — routes each request to the cheapest model that clears your quality bar",
		SignupURL:          "https://app.router.com/keys",
		BaseURL:            base,
		BaseURLEnv:         routerconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{routerconfig.APIKeyEnv, routerconfig.RouterAPIKeyEnv},
		Aliases:            []string{"ramp-router", "ramp", "router.com"},
		DefaultModel:       p.config.GetString(routerconfig.DefaultModel),
		Headers:            map[string]string{"User-Agent": "Icebath-Agent/dev"},
		ModelListing:       true,
		HealthCheck:        true,
		Vision:             true,
		VisionToolMessages: true,
		ModelsDevID:        routerconfig.Name,
		DefaultAuxModel:    "gpt-5.4-mini",
	}}
}
