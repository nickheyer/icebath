package provider

import "strings"

// Profiles the bundled ollama-cloud plugin registers
func ollamaCloudPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "ollama-cloud", keys: []string{"OLLAMA_API_KEY"}, base: "https://ollama.com/v1",
		authType: AuthTypeAPIKey}, "ollama_cloud")
	p.DefaultAuxModel = "nemotron-3-nano:30b"
	p.BuildAPIKwargsExtrasHook = ollamaCloudBuildAPIKwargsExtras
	return []Profile{p}
}

// Top level effort for thinking models with xhigh mapped to max, none when disabled and nothing for an unset effort
func ollamaCloudBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	supports, _ := kw["supports_reasoning"].(bool)
	if !supports || len(reasoningConfigMap(reasoning)) == 0 {
		return map[string]any{}, map[string]any{}
	}
	effort := strings.ToLower(strings.TrimSpace(reasoning.Effort))
	if reasoning.Enabled != nil && !*reasoning.Enabled || effort == "none" {
		return map[string]any{}, map[string]any{"reasoning_effort": "none"}
	}
	if effort == "" {
		return map[string]any{}, map[string]any{}
	}
	if clamped := clampEffort(effort, ollamaCloudEfforts, ollamaCloudOverrides); containsStr(ollamaCloudEfforts, clamped) {
		return map[string]any{}, map[string]any{"reasoning_effort": clamped}
	}
	return map[string]any{}, map[string]any{}
}
