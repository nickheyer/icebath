package provider

import (
	"regexp"
	"strings"
)

// Upstage models with no reasoning control at all
var upstageNonReasoningModelRE = regexp.MustCompile(`solar-mini(?:[-:.@]|$)|syn-pro`)

// Profiles the bundled upstage plugin registers
func upstagePluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "upstage", display: "Upstage Solar", desc: "Upstage (Solar API)",
		signup: "https://console.upstage.ai/api-keys", keys: []string{"UPSTAGE_API_KEY", "UPSTAGE_BASE_URL"},
		base: "https://api.upstage.ai/v1", authType: AuthTypeAPIKey, fallbacks: []string{"solar-pro3"}}, "solar")
	p.BuildAPIKwargsExtrasHook = upstageBuildAPIKwargsExtras
	return []Profile{p}
}

// Top level Solar effort, an unset config turning reasoning on at medium and explicit settings always winning
func upstageBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	if upstageNonReasoningModelRE.MatchString(strings.ToLower(strings.TrimSpace(hookString(kw, "model")))) {
		return map[string]any{}, map[string]any{}
	}
	rc := reasoningConfigMap(reasoning)
	if len(rc) == 0 {
		return map[string]any{}, map[string]any{"reasoning_effort": "medium"}
	}
	if enabled, ok := rc["enabled"].(bool); ok && !enabled {
		return map[string]any{}, map[string]any{}
	}
	effort := strings.ToLower(strings.TrimSpace(reasoning.Effort))
	if effort == "" {
		return map[string]any{}, map[string]any{"reasoning_effort": "medium"}
	}
	if effort == "minimal" {
		return map[string]any{}, map[string]any{}
	}
	mapped := clampEffort(effort, solarEfforts, nil)
	if !containsStr(solarEfforts, mapped) {
		mapped = ""
		if !containsStr(effortLadder, effort) {
			mapped = "high"
		}
	}
	if mapped == "" {
		return map[string]any{}, map[string]any{}
	}
	return map[string]any{}, map[string]any{"reasoning_effort": mapped}
}
