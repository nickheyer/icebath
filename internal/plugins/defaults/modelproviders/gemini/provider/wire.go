package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Total output a Gemini 4 request gets for an effort above high, over a decode that takes several hops
var longDecode = map[string]int{"xhigh": 262_144, "max": 1_048_576, "ultra": 1_048_576}

// Whether the address is AI Studio's OpenAI compatible surface
func compatible(base string) bool {
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(base), "/"))
	return strings.Contains(normalized, "generativelanguage.googleapis.com") && strings.HasSuffix(normalized, "/openai")
}

// Thinking config of the request: the snake case google block on the OpenAI compatible surface, the native config elsewhere, with the whole
// output a long Gemini 4 decode gets on the native API
func extraBody(call *provider.Call) map[string]any {
	raw := provider.GeminiThinking(call.Model, call.Ask)
	if raw == nil {
		return nil
	}
	if compatible(call.Route.BaseURL) {
		thinking := provider.GeminiSnake(raw)
		if thinking == nil {
			return nil
		}
		return map[string]any{"extra_body": map[string]any{"google": map[string]any{"thinking_config": thinking}}}
	}
	out := map[string]any{"thinking_config": raw}
	model := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(call.Model)), "google/")
	if budget := longDecode[call.Ask.Requested()]; budget > 0 && strings.HasPrefix(model, "gemini-4") && provider.NativeGemini(call.Route.BaseURL) {
		out["max_output_tokens"] = budget
	}
	return out
}

// The kind as one provider has it: Google's own Gemini API spoken natively, its OpenAI compatible surface over chat completions
func resolve(spec provider.Spec, _ config.View) provider.Spec {
	if provider.NativeGemini(spec.BaseURL) {
		spec.Mode = provider.ModeGeminiNative
	}
	return spec
}
