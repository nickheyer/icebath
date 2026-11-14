package providers

import (
	"context"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type tokenHubProfile struct {
	base
}

func newTokenHub() Profile {
	return &tokenHubProfile{base{spec: Spec{
		Name: "tencent-tokenhub", DisplayName: "Tencent TokenHub", SkipHealthCheck: true,
		EnvVars: []string{"TOKENHUB_API_KEY", "TOKENHUB_BASE_URL"},
	}}}
}

func (p *tokenHubProfile) DefaultReasoning(model string) *wire.Reasoning {
	return &wire.Reasoning{Enabled: true, Effort: "high"}
}

func (p *tokenHubProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	if f.Reasoning == nil || !f.Reasoning.Enabled {
		return Extras{}, nil
	}
	return effortTop(wire.TokenHubEffort(wire.RequestedEffort(f.Reasoning))), nil
}
