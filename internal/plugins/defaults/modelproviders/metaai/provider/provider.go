package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	metaaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/metaai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(metaaiconfig.BaseURL)
	return []provider.Profile{{
		Name:             metaaiconfig.Name,
		DisplayName:      "Meta Model API",
		Description:      "Meta Muse Spark family (Meta Superintelligence Labs)",
		SignupURL:        "https://developer.meta.com/ai/",
		BaseURL:          base,
		BaseURLEnv:       metaaiconfig.BaseURLEnv,
		ModelsURL:        provider.ModelsURL(base),
		Mode:             provider.ModeCodexResponses,
		Auth:             provider.AuthAPIKey,
		AuthType:         provider.AuthTypeAPIKey,
		KeyEnv:           []string{metaaiconfig.APIKeyEnv, metaaiconfig.MetaAPIKeyEnv, metaaiconfig.MetaModelAPIKeyEnv},
		Aliases:          []string{"meta", "muse", "muse-spark", "model-api", "msl"},
		DefaultModel:     p.config.GetString(metaaiconfig.DefaultModel),
		FallbackModels:   []string{"muse-spark-1.2"},
		ModelListing:     true,
		HealthCheck:      true,
		Vision:           true,
		DefaultMaxTokens: 16384,
		ModelsDevID:      "meta",
		DefaultAuxModel:  "muse-spark-1.2-contributor",
	}}
}
