package provider

import (
	nvidiaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := nvidiaconfig.BaseURL
	return []provider.Spec{{
		Name:               nvidiaconfig.Name,
		DisplayName:        nvidiaconfig.Title,
		Description:        "Hosted NIM inference",
		SignupURL:          "https://build.nvidia.com/",
		BaseURL:            base,
		BaseURLEnv:         nvidiaconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{nvidiaconfig.APIKeyEnv},
		Aliases:            []string{"nvidia-nim", "nim", "build-nvidia", "nemotron"},
		DefaultModel:       nvidiaconfig.DefaultModel,
		FallbackModels:     []string{"nvidia/llama-3.1-nemotron-70b-instruct", "nvidia/llama-3.3-70b-instruct"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		DefaultMaxTokens:   16384,
		ModelsDevID:        nvidiaconfig.Name,
	}}
}
