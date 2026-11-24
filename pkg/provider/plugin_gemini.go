package provider

import (
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Profiles the bundled gemini plugin registers
func geminiPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "gemini", mode: ModeChatCompletions, keys: []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"},
		base: "https://generativelanguage.googleapis.com/v1beta", authType: AuthTypeAPIKey}, "google", "google-gemini", "google-ai-studio")
	p.DefaultAuxModel = "gemini-3.6-flash"
	name, profileBase := p.Name, p.BaseURL
	p.BuildExtraBodyHook = func(_ string, kw map[string]any) *jsonutil.Object {
		return geminiBuildExtraBody(name, profileBase, kw)
	}
	return []Profile{p}
}

// Reasoning config a hook context map carries under reasoning_config, nil when absent
func hookReasoning(kw map[string]any) *Reasoning {
	switch r := kw["reasoning_config"].(type) {
	case *Reasoning:
		return r
	case Reasoning:
		return &r
	}
	return nil
}

// True for the AI Studio OpenAI compatible subpath
func geminiOpenAICompatBaseURL(baseURL string) bool {
	n := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	return n != "" && strings.Contains(n, "generativelanguage.googleapis.com") && strings.HasSuffix(n, "/openai")
}

// Native thinking_config with the long decode budget, or the snake case google block on the OpenAI compatible subpath
func geminiBuildExtraBody(name, profileBase string, kw map[string]any) *jsonutil.Object {
	model := hookString(kw, "model")
	reasoning := hookReasoning(kw)
	raw := buildGeminiThinkingConfig(model, hookReasoningCfg(reasoning))
	if len(raw) == 0 {
		return jsonutil.ObjectOf()
	}
	baseURL := hookString(kw, "base_url")
	if baseURL == "" {
		baseURL = profileBase
	}
	if name == "gemini" && geminiOpenAICompatBaseURL(baseURL) {
		thinking := snakeCaseGeminiThinkingConfig(raw)
		if len(thinking) == 0 {
			return jsonutil.ObjectOf()
		}
		return jsonutil.ObjectOf("extra_body", jsonutil.ObjectOf("google", jsonutil.ObjectOf("thinking_config", thinking)))
	}
	var rc map[string]any
	if reasoning != nil {
		rc = geminiReasoningConfig(*reasoning)
	}
	out := jsonutil.ObjectOf("thinking_config", raw)
	if budget := longDecodeMaxOutputTokens(model, rc); budget > 0 && isNativeGeminiBaseURL(baseURL) {
		out.Set("max_output_tokens", budget)
	}
	return out
}
