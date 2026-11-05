package provider

// Profiles the bundled tencent-tokenhub plugin registers, base URL left empty so listing keeps the generic paths
func tokenhubPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "tencent-tokenhub", display: "Tencent TokenHub", noHealth: true,
		keys: []string{"TOKENHUB_API_KEY", "TOKENHUB_BASE_URL"}, authType: AuthTypeAPIKey})
	p.DefaultReasoningConfigHook = func(string) *Reasoning {
		enabled := true
		return &Reasoning{Enabled: &enabled, Effort: "high"}
	}
	p.BuildAPIKwargsExtrasHook = tokenhubBuildAPIKwargsExtras
	return []Profile{p}
}

// Top level effort clamped to low medium high, high for an unset effort and nothing when thinking is off
func tokenhubBuildAPIKwargsExtras(reasoning *Reasoning, _ map[string]any) (map[string]any, map[string]any) {
	if len(reasoningConfigMap(reasoning)) == 0 || reasoning.Enabled != nil && !*reasoning.Enabled {
		return map[string]any{}, map[string]any{}
	}
	return map[string]any{}, map[string]any{"reasoning_effort": tokenhubEffort(requestedEffort(hookReasoningCfg(reasoning)))}
}
