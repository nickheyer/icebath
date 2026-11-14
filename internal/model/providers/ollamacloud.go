package providers

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type ollamaCloudProfile struct {
	base
}

func newOllamaCloud() Profile {
	return &ollamaCloudProfile{base{spec: Spec{
		Name: "ollama-cloud", Aliases: []string{"ollama_cloud"}, DefaultAuxModel: "nemotron-3-nano:30b",
		EnvVars: []string{"OLLAMA_API_KEY"}, BaseURL: "https://ollama.com/v1",
	}}}
}

func (p *ollamaCloudProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	r := f.Reasoning
	if !f.SupportsReasoning || r == nil {
		return Extras{}, nil
	}
	effort := reasoningEffortRaw(r)
	if !r.Enabled || effort == "none" {
		return effortTop("none"), nil
	}
	if effort == "" {
		return Extras{}, nil
	}
	family := wire.EffortsOllamaCloud.Efforts()
	clamped := wire.ClampEffort(effort, family.Levels, family.Round)
	if slices.Contains(family.Levels, clamped) {
		return effortTop(clamped), nil
	}
	return Extras{}, nil
}
