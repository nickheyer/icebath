package provider

import "context"

// Profiles the bundled nous plugin registers
func nousPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "nous", display: "Nous Research", desc: "Nous Research — Hermes model family",
		signup: "https://nousresearch.com/", keys: []string{"NOUS_API_KEY"}, base: "https://inference-api.nousresearch.com/v1",
		authType: AuthTypeOAuthDeviceCode, fallbacks: []string{"hermes-3-405b", "hermes-3-70b"},
	}, "nous-portal", "nousresearch")
	p.ResolveAuxModelHook = func(ctx context.Context, vision bool) string {
		return GetNousRecommendedAuxModel(ctx, vision, nil, "", false)
	}
	p.BuildExtraBodyHook = nousBuildExtraBody
	p.BuildAPIKwargsExtrasHook = nousBuildAPIKwargsExtras
	return []Profile{p}
}

// Set of strings a hook context key carries as a set, slice or map of flags
func hookStringSet(kw map[string]any, key string) map[string]bool {
	out := map[string]bool{}
	switch v := kw[key].(type) {
	case map[string]bool:
		for k, on := range v {
			if on {
				out[k] = true
			}
		}
	case []string:
		for _, s := range v {
			out[s] = true
		}
	case []any:
		for _, s := range v {
			if str, ok := s.(string); ok {
				out[str] = true
			}
		}
	}
	return out
}

// Portal product tags, the sticky routing key, and the data use routing fields an outgoing connector result requires
func nousBuildExtraBody(sessionID string, kw map[string]any) map[string]any {
	ctx := hookCtx(kw)
	tags := nousPortalTags(ctx, sessionID)
	anyTags := make([]any, len(tags))
	for i, tag := range tags {
		anyTags[i] = tag
	}
	body := map[string]any{"tags": anyTags}
	if sticky := cacheScopeFromSessionID(firstNonEmpty(portalAffinityScope(ctx), portalConversationContext(ctx), sessionID)); sticky != "" {
		body["session_id"] = sticky
	}
	if dataUse := hookStringSet(kw, "data_use"); len(dataUse) > 0 {
		provider := map[string]any{"data_collection": "deny"}
		if dataUse["zero_retention"] {
			provider["zdr"] = true
		}
		body["provider"] = provider
	}
	return body
}

// True when disabling reasoning would be rejected, a cold or unknown catalog entry warming the cache and answering true
func nousCannotDisableReasoning(ctx context.Context, model string) bool {
	caps, ok := NousModelReasoningCapabilities(ctx, model, 0, false)
	if !ok {
		WarmNousReasoningCapsAsync(ctx)
		return true
	}
	return !caps.SupportsReasoning || caps.Mandatory
}

// Full reasoning config passed through, a disable the model cannot honor omitted, medium effort when unset
func nousBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	if !pyTruthy(kw["supports_reasoning"]) {
		return map[string]any{}, map[string]any{}
	}
	if reasoning == nil {
		return map[string]any{"reasoning": map[string]any{"enabled": true, "effort": "medium"}}, map[string]any{}
	}
	rc := reasoningConfigMap(reasoning)
	if reasoning.Enabled != nil && !*reasoning.Enabled && nousCannotDisableReasoning(hookCtx(kw), hookString(kw, "model")) {
		return map[string]any{}, map[string]any{}
	}
	return map[string]any{"reasoning": rc}, map[string]any{}
}
