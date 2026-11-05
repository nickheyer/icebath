package provider

// Profiles the bundled fireworks plugin registers
func fireworksPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "fireworks", display: "Fireworks AI", desc: "Fireworks AI — OpenAI-compatible direct model API",
		signup: "https://app.fireworks.ai/settings/users/api-keys", keys: []string{"FIREWORKS_API_KEY"},
		base: "https://api.fireworks.ai/inference/v1", authType: AuthTypeAPIKey,
		fallbacks: []string{"accounts/fireworks/models/kimi-k2p6", "accounts/fireworks/models/glm-5p2", "accounts/fireworks/models/kimi-k2p7-code"},
	}, "fireworks-ai", "fw")
	p.LazyHeaders = attributionLazyHeaders("IcebathAgent")
	p.DefaultAuxModel = "accounts/fireworks/models/glm-5p2"
	p.BuildAPIKwargsExtrasHook = fireworksBuildAPIKwargsExtras
	return []Profile{p}
}

// Top level effort as sent, none when thinking is disabled and nothing without a reasoning config
func fireworksBuildAPIKwargsExtras(reasoning *Reasoning, _ map[string]any) (map[string]any, map[string]any) {
	if reasoning == nil {
		return map[string]any{}, map[string]any{}
	}
	if reasoning.Enabled != nil && !*reasoning.Enabled {
		return map[string]any{}, map[string]any{"reasoning_effort": "none"}
	}
	if reasoning.Effort == "" {
		return map[string]any{}, map[string]any{}
	}
	return map[string]any{}, map[string]any{"reasoning_effort": reasoning.Effort}
}
