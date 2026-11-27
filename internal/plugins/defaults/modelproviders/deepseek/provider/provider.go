package provider

import (
	deepseekconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepseek/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:                       deepseekconfig.Name,
		Shape:                      shape,
		EchoesReasoning:            true,
		Native:                     native,
		DisplayName:                deepseekconfig.Title,
		Description:                "DeepSeek models, with an API key paid by use",
		SignupURL:                  "https://platform.deepseek.com/",
		BaseURL:                    deepseekconfig.BaseURL,
		BaseURLEnv:                 deepseekconfig.BaseURLEnv,
		ModelsURL:                  deepseekconfig.ModelsURL,
		Mode:                       provider.ModeChatCompletions,
		Auth:                       provider.AuthAPIKey,
		AuthType:                   provider.AuthTypeAPIKey,
		KeyEnv:                     []string{deepseekconfig.APIKeyEnv},
		Aliases:                    []string{"deepseek-chat", "deep-seek"},
		DefaultModel:               deepseekconfig.DefaultModel,
		DefaultAuxModel:            "deepseek-flash",
		FallbackModels:             []string{"deepseek-v4-pro", "deepseek-flash"},
		ModelListing:               true,
		HealthCheck:                true,
		VisionToolMessages:         true,
		UnsupportedResponseFormats: []string{"json_schema"},
		ModelsDevID:                deepseekconfig.Name,
		StaticModels:               []string{"deepseek-flash", "deepseek-v4-pro"},
		ModelMetadata:              flashMetadata(),
		Prices:                     prices,
		ListPrices:                 "deepseek.com",
		Billing:                    []provider.Billing{{Label: "DeepSeek", URL: "https://platform.deepseek.com/top_up"}},
	}}
}
