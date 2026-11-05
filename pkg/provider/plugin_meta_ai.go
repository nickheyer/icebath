package provider

import (
	"context"
	"os"
	"strings"
	"time"
)

// Live catalog prefixes of image generation and transcription models kept out of the agent picker
var metaAINonChatPrefixes = []string{"muse-image-", "muse-voice-"}

// Profiles the bundled meta-ai plugin registers
func metaAIPluginProfiles() []Profile {
	base := strings.TrimSpace(os.Getenv("META_BASE_URL"))
	if base == "" {
		base = "https://api.meta.ai/v1"
	}
	p := bundledRowProfile(profileRow{name: "meta-ai", display: "Meta Model API", desc: "Meta Muse Spark family (Meta Superintelligence Labs)",
		signup: "https://developer.meta.com/ai/", keys: []string{"MODEL_API_KEY", "META_API_KEY", "META_MODEL_API_KEY", "META_BASE_URL"},
		base: base, authType: AuthTypeAPIKey, mode: ModeCodexResponses, vision: true, noVisionTool: true, maxTokens: 16384,
		fallbacks: []string{"muse-spark-1.2"},
	}, "meta", "muse", "muse-spark", "model-api", "msl")
	p.DefaultAuxModel = "muse-spark-1.2-contributor"
	plain := p
	p.FetchModelsHook = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
		live, ok := plain.FetchModels(ctx, apiKey, baseURL, timeout)
		if !ok {
			return nil, false
		}
		out := []string{}
		for _, m := range live {
			if !hasAnyPrefix(m, metaAINonChatPrefixes) {
				out = append(out, m)
			}
		}
		return out, true
	}
	p.BuildAPIKwargsExtrasHook = metaAIBuildAPIKwargsExtras
	return []Profile{p}
}

// Top level effort always sent, off or none mapping to minimal and unset or bespoke levels to medium
func metaAIBuildAPIKwargsExtras(reasoning *Reasoning, _ map[string]any) (map[string]any, map[string]any) {
	r := Reasoning{}
	if reasoning != nil {
		r = *reasoning
	}
	effort := strings.ToLower(strings.TrimSpace(r.Effort))
	mapped := "medium"
	if r.Enabled != nil && !*r.Enabled || effort == "none" {
		mapped = "minimal"
	} else if clamped := clampEffort(effort, metaAIEfforts, nil); containsStr(metaAIEfforts, clamped) {
		mapped = clamped
	}
	return map[string]any{}, map[string]any{"reasoning_effort": mapped}
}
