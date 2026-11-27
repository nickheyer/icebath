package provider

import (
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               openrouterconfig.Name,
		AutoFirst:          true,
		Settings:           settings(),
		ExtraBody:          extraBody,
		Shape:              shape,
		RequestHeaders:     headers,
		Carriers:           []string{"reasoning_content", "reasoning", "reasoning_details"},
		Native:             provider.Vendored,
		DisplayName:        openrouterconfig.Title,
		Rank:               6,
		Description:        "Many companies' models behind one API key or OpenRouter sign-in",
		SignupURL:          "https://openrouter.ai/keys",
		BaseURL:            openrouterconfig.BaseURL,
		BaseURLEnv:         openrouterconfig.BaseURLEnv,
		ModelsURL:          openrouterconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{openrouterconfig.APIKeyEnv},
		Aliases:            []string{"or", "openai"},
		DefaultModel:       openrouterconfig.DefaultModel,
		FallbackModels:     []string{"anthropic/claude-sonnet-4.6", "openai/gpt-5.4", "deepseek/deepseek-chat", "google/gemini-3.8-flash", "google/gemini-3.7-flash", "qwen/qwen3-plus"},
		Headers:            map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent"},
		Aggregator:         true,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        openrouterconfig.Name,
		StaticModels: []string{"anthropic/claude-fable-5.1", "anthropic/claude-fable-5", "anthropic/claude-opus-5.5",
			"anthropic/claude-opus-5", "anthropic/claude-opus-5-fast", "anthropic/claude-opus-4.8", "anthropic/claude-opus-4.8-fast",
			"anthropic/claude-sonnet-5.5", "anthropic/claude-sonnet-5", "anthropic/claude-haiku-5.5", "anthropic/claude-haiku-4.5",
			"openai/gpt-6-astra", "openai/gpt-6-astra-fast", "openai/gpt-6-astra-flex", "openai/gpt-6-astra-pro",
			"openai/gpt-6-astra-pro-fast", "openai/gpt-6-astra-pro-flex",
			"openai/gpt-6.1-sol", "openai/gpt-6.1-sol-pro", "openai/gpt-6.1-terra", "openai/gpt-6-sol", "openai/gpt-6-sol-pro",
			"openai/gpt-6-luna", "openai/gpt-6-luna-pro",
			"openai/gpt-5.5", "openai/gpt-5.5-pro", "openai/gpt-5.4-mini", "google/gemini-4-argon",
			"google/gemini-3.1-pro-preview",
			"google/gemini-3.8-flash", "google/gemini-3.7-flash", "x-ai/grok-4.7", "x-ai/grok-4.6",
			"deepseek/deepseek-v4-pro",
			"deepseek/deepseek-v4-pro-0813", "deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4.1-flash-1017",
			"deepseek/deepseek-v4-flash-0731",
			"qwen/qwen3.8-max-0902", "qwen/qwen3.8-flash", "moonshotai/kimi-k3", "minimax/minimax-m3", "z-ai/glm-5.3",
			"z-ai/glm-5.3-flash", "z-ai/glm-5.3-flashx", "z-ai/glm-5.2",
			"xiaomi/mimo-v2.6-pro", "xiaomi/mimo-v2.6-flash", "xiaomi/mimo-v2.6-pro-ultraspeed", "xiaomi/mimo-v2.5-pro", "tencent/hy4-preview",
			"tencent/hy3",
			"stepfun/step-3.7-flash", "nvidia/nemotron-3-super-120b-a12b", "meta/muse-spark-1.2",
			"meta/muse-spark-1.2-contributor", "meta/muse-spark-1.3", "meta/muse-spark-1.3-contributor", "sakana/fugu-ultra",
			"upstage/solar-pro4", "upstage/solar-mini4",
			"openrouter/pareto-code", "thinkingmachines/inkling:free", "thinkingmachines/inkling-small:free",
			"minimax/minimax-m3:free", "z-ai/glm-5.2:free", "poolside/laguna-s-2.1:free", "poolside/laguna-xs-2.1:free",
			"nvidia/nemotron-3-super-120b-a12b:free", "nvidia/nemotron-3-ultra-550b-a55b:free",
			"inclusionai/ling-3.0-flash:free", "nvidia/nemotron-3.5-lightning:free", "stealth/union-alpha",
			"stealth/space-bunny-alpha"},
		CatalogDefault: "z-ai/glm-5.3",
		Billing:        []provider.Billing{{Label: "OpenRouter", URL: "https://openrouter.ai/settings/credits"}},
		AccountUsage:   accountUsage,
	}}
}
