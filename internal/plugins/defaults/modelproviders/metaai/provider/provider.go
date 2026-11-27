package provider

import (
	metaaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/metaai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := metaaiconfig.BaseURL
	return []provider.Spec{{
		Name:             metaaiconfig.Name,
		Shape:            shape,
		DisplayName:      metaaiconfig.Title,
		Description:      "Muse Spark models",
		SignupURL:        "https://developer.meta.com/ai/",
		BaseURL:          base,
		BaseURLEnv:       metaaiconfig.BaseURLEnv,
		ModelsURL:        provider.ModelsURL(base),
		Mode:             provider.ModeCodexResponses,
		Auth:             provider.AuthAPIKey,
		AuthType:         provider.AuthTypeAPIKey,
		KeyEnv:           []string{metaaiconfig.APIKeyEnv, metaaiconfig.MetaAPIKeyEnv, metaaiconfig.MetaModelAPIKeyEnv},
		Aliases:          []string{"meta", "muse", "muse-spark", "model-api", "msl"},
		DefaultModel:     metaaiconfig.DefaultModel,
		FallbackModels:   []string{"muse-spark-1.2"},
		ModelListing:     true,
		HealthCheck:      true,
		Vision:           true,
		DefaultMaxTokens: 16384,
		ModelsDevID:      "meta",
		LiveFirst:        true,
		DefaultAuxModel:  "muse-spark-1.2-contributor",
	}}
}
