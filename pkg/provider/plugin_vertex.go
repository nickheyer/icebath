package provider

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Profiles the bundled vertex plugin registers, the real base URL minted at runtime with the OAuth token
func vertexPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "vertex", mode: ModeChatCompletions, base: "https://aiplatform.googleapis.com",
		authType: AuthTypeVertex, noListing: true}, "google-vertex", "vertex-ai", "gcp-vertex", "vertexai")
	p.DefaultAuxModel = "google/gemini-3.6-flash"
	p.BuildExtraBodyHook = vertexBuildExtraBody
	return []Profile{p}
}

// Snake case google thinking_config in extra_body like the gemini OpenAI compatible subpath
func vertexBuildExtraBody(_ string, kw map[string]any) *jsonutil.Object {
	raw := buildGeminiThinkingConfig(hookString(kw, "model"), hookReasoningCfg(hookReasoning(kw)))
	if len(raw) == 0 {
		return jsonutil.ObjectOf()
	}
	thinking := snakeCaseGeminiThinkingConfig(raw)
	if len(thinking) == 0 {
		return jsonutil.ObjectOf()
	}
	return jsonutil.ObjectOf("extra_body", jsonutil.ObjectOf("google", jsonutil.ObjectOf("thinking_config", thinking)))
}
