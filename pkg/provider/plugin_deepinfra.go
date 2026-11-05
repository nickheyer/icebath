package provider

import (
	"context"
	"strings"
	"time"
)

// Profiles the bundled deepinfra plugin registers
func deepinfraPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "deepinfra", display: "DeepInfra", desc: "DeepInfra — 100+ open models, pay-per-use",
		signup: "https://deepinfra.com/dash/api_keys", keys: []string{"DEEPINFRA_API_KEY", "DEEPINFRA_BASE_URL"},
		base: "https://api.deepinfra.com/v1/openai", authType: AuthTypeAPIKey, fallbacks: []string{}}, "deep-infra", "deepinfra-ai")
	p.DefaultAuxModel = "deepseek-ai/DeepSeek-V4-Flash"
	p.BuildAPIKwargsExtrasHook = deepinfraBuildAPIKwargsExtras
	p.DefaultVisionModelHook = deepinfraDefaultVisionModel
	return []Profile{p}
}

// Top level effort on the OpenAI compatible vocabulary, none the only off switch and an unset effort omitted
func deepinfraBuildAPIKwargsExtras(reasoning *Reasoning, _ map[string]any) (map[string]any, map[string]any) {
	if reasoning != nil && reasoning.Enabled != nil && !*reasoning.Enabled {
		return map[string]any{}, map[string]any{"reasoning_effort": "none"}
	}
	clamped := clampEffort(requestedEffort(hookReasoningCfg(reasoning)), openAICompatWireEfforts, nil)
	if containsStr(openAICompatWireEfforts, clamped) {
		return map[string]any{}, map[string]any{"reasoning_effort": clamped}
	}
	return map[string]any{}, map[string]any{}
}

// Seam the vision default reads the live catalog through
var deepinfraModelsByTagFn = fetchDeepinfraModelsByTagIn

// First vision tagged chat model of the live catalog, empty without a DeepInfra key or on a failed fetch
func deepinfraDefaultVisionModel(ctx context.Context) string {
	if strings.TrimSpace(GetSecretStr(ctx, "DEEPINFRA_API_KEY", "")) == "" {
		return ""
	}
	items, ok := deepinfraModelsByTagFn(ctx, "chat", 5*time.Second, false)
	if !ok {
		return ""
	}
	for _, item := range items {
		meta, isMap := item["metadata"].(map[string]any)
		if !isMap {
			continue
		}
		tags, isList := meta["tags"].([]any)
		if !isList || !pyTruthy(item["id"]) {
			continue
		}
		for _, tag := range tags {
			if tag == "vision" {
				return pyStr(item["id"])
			}
		}
	}
	return ""
}
