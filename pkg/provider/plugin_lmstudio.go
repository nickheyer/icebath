package provider

// Profiles the bundled lmstudio plugin registers, base URL left empty so loopback hosts never infer LM Studio
func lmstudioPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "lmstudio", display: "LM Studio", noListing: true, noHealth: true,
		keys: []string{"LM_API_KEY", "LM_BASE_URL"}, authType: AuthTypeAPIKey})
	p.BuildAPIKwargsExtrasHook = lmstudioBuildAPIKwargsExtras
	return []Profile{p}
}

// Published reasoning options a hook context map carries, false when the caller ran no probe
func hookLMStudioOptions(kw map[string]any) ([]string, bool) {
	switch v := kw["lmstudio_reasoning_options"].(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, o := range v {
			out = append(out, pyStr(o))
		}
		return out, true
	}
	return nil, false
}

// Top level effort clamped to the model's published options, the generic nested shape when no probe ran
func lmstudioBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	options, probed := hookLMStudioOptions(kw)
	if !probed {
		return genericNestedReasoning(hookReasoningCfg(reasoning)), map[string]any{}
	}
	if supports, _ := kw["supports_reasoning"].(bool); !supports {
		return map[string]any{}, map[string]any{}
	}
	if effort, ok := resolveLMStudioEffort(hookReasoningCfg(reasoning), options); ok {
		return map[string]any{}, map[string]any{"reasoning_effort": effort}
	}
	return map[string]any{}, map[string]any{}
}
