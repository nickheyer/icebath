package provider

import (
	xiaomiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := xiaomiconfig.BaseURL
	return []provider.Spec{{
		Name:              xiaomiconfig.Name,
		DottedModels:      true,
		VisionHelper:      "mimo-v2.5",
		EchoesReasoning:   true,
		Native:            native,
		DisplayName:       xiaomiconfig.Title,
		BaseURL:           base,
		BaseURLEnv:        xiaomiconfig.BaseURLEnv,
		ModelsURL:         provider.ModelsURL(base),
		Mode:              provider.ModeChatCompletions,
		Auth:              provider.AuthAPIKey,
		AuthType:          provider.AuthTypeAPIKey,
		KeyEnv:            []string{xiaomiconfig.APIKeyEnv},
		Aliases:           []string{"mimo", "xiaomi-mimo"},
		DefaultModel:      xiaomiconfig.DefaultModel,
		ModelListing:      true,
		Vision:            true,
		ModelsDevID:       xiaomiconfig.Name,
		StaticModels:      []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed", "mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-pro", "mimo-v2-omni", "mimo-v2-flash"},
		MetadataHostnames: []string{"xiaomimimo.com"},
		ListPrices:        "xiaomimimo.com",
	}}
}
