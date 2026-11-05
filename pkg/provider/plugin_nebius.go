package provider

import "strings"

// Model name fragments of the Nebius reasoning families
var nebiusReasoningMarkers = []string{"deepseek-r1", "deepseek-v4", "deepseek-reasoner", "gpt-oss", "glm-5", "kimi-k2", "minimax-m2", "qwen3"}

// Profiles the bundled nebius-token-factory plugin registers
func nebiusPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "nebius-token-factory", display: "Nebius Token Factory",
		desc: "Nebius Token Factory — OpenAI-compatible inference", signup: "https://tokenfactory.nebius.com/",
		keys: []string{"NEBIUS_API_KEY", "NEBIUS_TOKEN_FACTORY_API_KEY", "NEBIUS_BASE_URL"}, base: "https://api.tokenfactory.nebius.com/v1",
		modelsURL: "https://api.tokenfactory.nebius.com/v1/models?verbose=true", authType: AuthTypeAPIKey,
		fallbacks: []string{"Qwen/Qwen3.5-397B-A17B-fast", "deepseek-ai/DeepSeek-V4-Pro", "zai-org/GLM-5.1", "moonshotai/Kimi-K2.5-fast",
			"MiniMaxAI/MiniMax-M2.5-fast", "deepseek-ai/DeepSeek-V3.2-fast", "NousResearch/Hermes-4-70B",
			"openai/gpt-oss-120b-fast", "meta-llama/Llama-3.3-70B-Instruct"},
	}, "nebius", "nebius-tokenfactory", "nebius-tf", "token-factory", "tokenfactory")
	p.DefaultAuxModel = "nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B"
	p.BuildAPIKwargsExtrasHook = nebiusBuildAPIKwargsExtras
	return []Profile{p}
}

// Top level effort for reasoning families clamped onto low medium high, off words and a disabled config sending nothing
func nebiusBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	model := strings.TrimSpace(hookString(kw, "model"))
	name := strings.ToLower(model[strings.LastIndex(model, "/")+1:])
	supports, _ := kw["supports_reasoning"].(bool)
	if !supports && !containsAnySub(name, nebiusReasoningMarkers...) {
		return map[string]any{}, map[string]any{}
	}
	effort, enabled := "medium", true
	if reasoning != nil {
		if reasoning.Effort != "" {
			effort = strings.ToLower(strings.TrimSpace(reasoning.Effort))
		}
		if reasoning.Enabled != nil {
			enabled = *reasoning.Enabled
		}
	}
	if !enabled || effort == "none" || effort == "off" || effort == "disabled" {
		return map[string]any{}, map[string]any{}
	}
	clamped := clampEffort(effort, nebiusEfforts, nil)
	if clamped == "" {
		clamped = "medium"
	}
	return map[string]any{}, map[string]any{"reasoning_effort": clamped}
}
