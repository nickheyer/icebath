package provider

import (
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Profiles the bundled ollama-cloud plugin registers
func ollamaCloudPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "ollama-cloud", keys: []string{"OLLAMA_API_KEY"}, base: "https://ollama.com/v1",
		authType: AuthTypeAPIKey}, "ollama_cloud")
	p.DefaultAuxModel = "nemotron-3-nano:30b"
	p.BuildAPIKwargsExtrasHook = ollamaCloudBuildAPIKwargsExtras
	return []Profile{p}
}

// Top level effort for thinking models with xhigh mapped to max, none when disabled and nothing for an unset effort
func ollamaCloudBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (*jsonutil.Object, map[string]any) {
	supports, _ := kw["supports_reasoning"].(bool)
	if !supports || len(reasoningConfigMap(reasoning)) == 0 {
		return jsonutil.ObjectOf(), map[string]any{}
	}
	effort := strings.ToLower(strings.TrimSpace(reasoning.Effort))
	if reasoning.Enabled != nil && !*reasoning.Enabled || effort == "none" {
		return jsonutil.ObjectOf(), map[string]any{"reasoning_effort": "none"}
	}
	if effort == "" {
		return jsonutil.ObjectOf(), map[string]any{}
	}
	if clamped := clampEffort(effort, ollamaCloudEfforts, ollamaCloudOverrides); containsStr(ollamaCloudEfforts, clamped) {
		return jsonutil.ObjectOf(), map[string]any{"reasoning_effort": clamped}
	}
	return jsonutil.ObjectOf(), map[string]any{}
}
