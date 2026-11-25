package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	nvidiaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(nvidiaconfig.BaseURL)
	return []provider.Profile{{
		Name:               nvidiaconfig.Name,
		DisplayName:        "NVIDIA NIM",
		Description:        "NVIDIA NIM - accelerated inference",
		SignupURL:          "https://build.nvidia.com/",
		BaseURL:            base,
		BaseURLEnv:         nvidiaconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{nvidiaconfig.APIKeyEnv},
		Aliases:            []string{"nvidia-nim", "nim", "build-nvidia", "nemotron"},
		DefaultModel:       p.config.GetString(nvidiaconfig.DefaultModel),
		FallbackModels:     []string{"nvidia/llama-3.1-nemotron-70b-instruct", "nvidia/llama-3.3-70b-instruct"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		DefaultMaxTokens:   16384,
		ModelsDevID:        nvidiaconfig.Name,
	}}
}
