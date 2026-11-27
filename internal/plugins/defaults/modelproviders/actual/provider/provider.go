package provider

import (
	actualconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := actualconfig.BaseURL
	return []provider.Spec{{
		Name:               actualconfig.Name,
		Shape:              shape,
		Efforts:            efforts,
		DisplayName:        actualconfig.Title,
		Description:        "Actual Computer's models, or your own server, with an API key",
		SignupURL:          "https://actual.inc",
		BaseURL:            base,
		BaseURLEnv:         actualconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(NormalizeBaseURL(base)),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{actualconfig.APIKeyEnv},
		Aliases:            []string{"actual-computer", "actualcomputer", "aci"},
		DefaultModel:       actualconfig.DefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
	}}
}
