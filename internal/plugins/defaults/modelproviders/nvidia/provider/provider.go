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
		Messages:           messages,
		RequestHeaders:     headers,
		Native:             native,
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
		StaticModels:       []string{"nvidia/nemotron-3.5-super-vl-120b-a12b", "nvidia/nemotron-3-ultra-550b-a55b", "nvidia/nemotron-3-super-120b-a12b", "nvidia/nemotron-3.5-lightning-30b-a3b", "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning", "z-ai/glm-5.3", "z-ai/glm-5.2", "moonshotai/kimi-k2.6", "minimaxai/minimax-m3"},
		ModelsDevPreferred: true,
		ContextLength:      contextLength,
		Billing:            []provider.Billing{{Label: "NVIDIA", URL: "https://build.nvidia.com/settings/billing"}},
	}}
}
