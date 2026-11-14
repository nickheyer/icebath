package providers

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

var nebiusReasoningMarkers = []string{"deepseek-r1", "deepseek-v4", "deepseek-reasoner", "gpt-oss", "glm-5", "kimi-k2", "minimax-m2", "qwen3"}

type nebiusProfile struct {
	base
}

func newNebius() Profile {
	return &nebiusProfile{base{spec: Spec{
		Name:        "nebius-token-factory",
		Aliases:     []string{"nebius", "nebius-tokenfactory", "nebius-tf", "token-factory", "tokenfactory"},
		DisplayName: "Nebius Token Factory", Description: "Nebius Token Factory — OpenAI-compatible inference",
		SignupURL: "https://tokenfactory.nebius.com/",
		EnvVars:   []string{"NEBIUS_API_KEY", "NEBIUS_TOKEN_FACTORY_API_KEY", "NEBIUS_BASE_URL"},
		BaseURL:   "https://api.tokenfactory.nebius.com/v1",
		ModelsURL: "https://api.tokenfactory.nebius.com/v1/models?verbose=true", AuthType: authTypeAPIKey,
		DefaultAuxModel: "nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B",
		FallbackModels: []string{"Qwen/Qwen3.5-397B-A17B-fast", "deepseek-ai/DeepSeek-V4-Pro", "zai-org/GLM-5.1", "moonshotai/Kimi-K2.5-fast",
			"MiniMaxAI/MiniMax-M2.5-fast", "deepseek-ai/DeepSeek-V3.2-fast",
			"openai/gpt-oss-120b-fast", "meta-llama/Llama-3.3-70B-Instruct"},
	}}}
}

func (p *nebiusProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	name := flatModelName(f.Model)
	if !f.SupportsReasoning && !slices.ContainsFunc(nebiusReasoningMarkers, func(m string) bool { return strings.Contains(name, m) }) {
		return Extras{}, nil
	}
	effort := "medium"
	enabled := true
	if r := f.Reasoning; r != nil {
		enabled = r.Enabled
		if e := reasoningEffortRaw(r); e != "" {
			effort = e
		}
	}
	if !enabled || effort == "none" || effort == "off" || effort == "disabled" {
		return Extras{}, nil
	}
	clamped := wire.ClampEffort(effort, wire.EffortsNebius.Efforts().Levels, nil)
	if clamped == "" {
		clamped = "medium"
	}
	return effortTop(clamped), nil
}
