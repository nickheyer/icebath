package providers

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

var upstageNonReasoning = regexp.MustCompile(`solar-mini(?:[-:.@]|$)|syn-pro`)

type upstageProfile struct {
	base
}

func newUpstage() Profile {
	return &upstageProfile{base{spec: Spec{
		Name: "upstage", Aliases: []string{"solar"}, DisplayName: "Upstage Solar", Description: "Upstage (Solar API)",
		SignupURL: "https://console.upstage.ai/api-keys", EnvVars: []string{"UPSTAGE_API_KEY", "UPSTAGE_BASE_URL"},
		BaseURL: "https://api.upstage.ai/v1", AuthType: authTypeAPIKey, FallbackModels: []string{"solar-pro3"},
	}}}
}

func (p *upstageProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	if upstageNonReasoning.MatchString(strings.ToLower(strings.TrimSpace(f.Model))) {
		return Extras{}, nil
	}
	r := f.Reasoning
	if r == nil {
		return effortTop("medium"), nil
	}
	if !r.Enabled {
		return Extras{}, nil
	}
	effort := reasoningEffortRaw(r)
	switch effort {
	case "":
		return effortTop("medium"), nil
	case "minimal":
		return Extras{}, nil
	}
	levels := wire.EffortsSolar.Efforts().Levels
	mapped := wire.ClampEffort(effort, levels, nil)
	if !slices.Contains(levels, mapped) {
		if slices.Contains(wire.EffortLadder(), effort) {
			return Extras{}, nil
		}
		mapped = "high"
	}
	return effortTop(mapped), nil
}
