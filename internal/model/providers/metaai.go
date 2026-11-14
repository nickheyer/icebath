package providers

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

var metaNonChatPrefixes = []string{"muse-image-", "muse-voice-"}

type metaAIProfile struct {
	base
	d Deps
}

func newMetaAI(d Deps) Profile {
	baseURL := strings.TrimSpace(os.Getenv("META_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://api.meta.ai/v1"
	}
	return &metaAIProfile{d: d, base: base{spec: Spec{
		Name: "meta-ai", Aliases: []string{"meta", "muse", "muse-spark", "model-api", "msl"}, DisplayName: "Meta Model API",
		Description: "Meta Muse Spark family (Meta Superintelligence Labs)", SignupURL: "https://developer.meta.com/ai/",
		EnvVars: []string{"MODEL_API_KEY", "META_API_KEY", "META_MODEL_API_KEY", "META_BASE_URL"},
		BaseURL: baseURL, AuthType: authTypeAPIKey, Mode: wire.ModeResponses,
		Vision: true, RejectsVisionToolMessages: true, DefaultAuxModel: "muse-spark-1.2-contributor",
		DefaultMaxTokens: 16384, FallbackModels: []string{"muse-spark-1.2"},
	}}}
}

func (p *metaAIProfile) ListModels(ctx context.Context, in ListInput) ([]string, bool) {
	live, ok := FetchModels(ctx, p.d.Net, &p.spec, in)
	if !ok {
		return nil, false
	}
	kept := []string{}
	for _, m := range live {
		if !slices.ContainsFunc(metaNonChatPrefixes, func(prefix string) bool { return strings.HasPrefix(m, prefix) }) {
			kept = append(kept, m)
		}
	}
	return kept, true
}

func (p *metaAIProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	effort := reasoningEffortRaw(f.Reasoning)
	mapped := "medium"
	if (f.Reasoning != nil && !f.Reasoning.Enabled) || effort == "none" {
		mapped = "minimal"
	} else {
		levels := wire.EffortsMetaAI.Efforts().Levels
		if clamped := wire.ClampEffort(effort, levels, nil); slices.Contains(levels, clamped) {
			mapped = clamped
		}
	}
	return effortTop(mapped), nil
}
