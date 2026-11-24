package provider

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Profiles the bundled ai-gateway plugin registers
func aiGatewayPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "ai-gateway", keys: []string{"AI_GATEWAY_API_KEY"}, base: "https://ai-gateway.vercel.sh/v1",
		authType: AuthTypeAPIKey, headers: map[string]string{"HTTP-Referer": attributionReferer, "X-Title": attributionTitle}},
		"vercel", "vercel-ai-gateway", "ai_gateway", "aigateway")
	p.DefaultAuxModel = "google/gemini-3-flash"
	p.BuildAPIKwargsExtrasHook = aiGatewayBuildAPIKwargsExtras
	return []Profile{p}
}

// Reasoning passed through as sent, enabled at medium when unset and nothing when the model cannot reason
func aiGatewayBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (*jsonutil.Object, map[string]any) {
	if supports, ok := kw["supports_reasoning"].(bool); ok && !supports {
		return jsonutil.ObjectOf(), map[string]any{}
	}
	cfg := jsonutil.ObjectOf("enabled", true, "effort", "medium")
	if reasoning != nil {
		cfg = reasoningConfigObject(reasoning)
	}
	return jsonutil.ObjectOf("reasoning", cfg), map[string]any{}
}
