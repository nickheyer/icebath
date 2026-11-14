package providers

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type deepInfraProfile struct {
	base
	d Deps
}

func newDeepInfra(d Deps) Profile {
	return &deepInfraProfile{d: d, base: base{spec: Spec{
		Name: "deepinfra", Aliases: []string{"deep-infra", "deepinfra-ai"}, DisplayName: "DeepInfra",
		Description: "DeepInfra — 100+ open models, pay-per-use", SignupURL: "https://deepinfra.com/dash/api_keys",
		EnvVars: []string{"DEEPINFRA_API_KEY", "DEEPINFRA_BASE_URL"}, BaseURL: "https://api.deepinfra.com/v1/openai",
		AuthType: authTypeAPIKey, DefaultAuxModel: "deepseek-ai/DeepSeek-V4-Flash", FallbackModels: []string{},
	}}}
}

func (p *deepInfraProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	if f.Reasoning != nil && !f.Reasoning.Enabled {
		return effortTop("none"), nil
	}
	levels := wire.EffortsOpenAICompat.Efforts().Levels
	clamped := wire.ClampEffort(wire.RequestedEffort(f.Reasoning), levels, nil)
	if slices.Contains(levels, clamped) {
		return effortTop(clamped), nil
	}
	return Extras{}, nil
}

func (p *deepInfraProfile) VisionModel(ctx context.Context) (string, bool) {
	if strings.TrimSpace(p.d.secret(ctx, "DEEPINFRA_API_KEY")) == "" || p.d.Catalog == nil {
		return "", false
	}
	items, ok := p.d.Catalog.DeepInfraModelsByTag(ctx, "chat")
	if !ok {
		return "", false
	}
	for _, item := range items {
		tags := item.Get("metadata").Get("tags")
		if !tags.IsArray() {
			continue
		}
		vision := slices.ContainsFunc(tags.Elems(), func(t wire.Value) bool { s, ok := t.Str(); return ok && s == "vision" })
		if id := item.Get("id"); vision && id.Truthy() {
			return id.Display(), true
		}
	}
	return "", false
}
